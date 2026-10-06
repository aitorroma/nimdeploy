package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Payment platforms: run a command when a payment event arrives (checkout
// completed, subscription renewed...). They all retry failed deliveries, so
// a full queue answers 503 (they'll retry) while invalid events get 200
// (retrying wouldn't fix them) plus a log line and a notification.
//
//   stripe        Stripe-Signature: t=<unix>,v1=<hex HMAC of "<t>.<body>">  (key: the whole whsec_... secret)
//   paddle        Paddle-Signature: ts=<unix>;h1=<hex HMAC of "<ts>:<body>">  (Paddle Billing)
//   lemonsqueezy  X-Signature: <hex HMAC of the body>, X-Event-Name

type paymentHandler struct {
	verify func(d *DeployConfig, r *http.Request, body []byte, now time.Time) error
	// event returns the event's unique ID (for duplicates), its type and the
	// ID of the object it is about.
	event func(r *http.Request, doc any) (id, typ, resource string)
}

var paymentProviders = map[string]paymentHandler{
	"stripe":       {verify: verifyStripe, event: stripeEvent},
	"paddle":       {verify: verifyPaddle, event: paddleEvent},
	"lemonsqueezy": {verify: verifyLemonSqueezy, event: lemonSqueezyEvent},
}

func paymentProvider(name string) bool {
	_, ok := paymentProviders[name]
	return ok
}

func (d *DeployConfig) validatePayment() error {
	if d.Branch != "" {
		return fmt.Errorf("branch does not apply to provider = %q (use events or when)", d.Provider)
	}
	for _, e := range d.Events {
		if strings.TrimSpace(e) == "" || strings.ContainsAny(e, " \t") {
			return fmt.Errorf("events: %q is not an event type", e)
		}
	}
	if d.MaxSkew.Duration < 0 {
		return fmt.Errorf("max_skew must be positive")
	}
	if d.MaxSkew.Duration == 0 {
		d.MaxSkew.Duration = defaultMaxSkew
	}
	return nil
}

// signedHeader parses "k=v<sep>k=v..." into the timestamp and the signatures
// under sigKey (there can be several while a secret is being rotated).
func signedHeader(header, sep, tsKey, sigKey string) (ts string, sigs [][]byte) {
	for _, part := range strings.Split(header, sep) {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case tsKey:
			ts = v
		case sigKey:
			if b, err := hex.DecodeString(v); err == nil {
				sigs = append(sigs, b)
			}
		}
	}
	return ts, sigs
}

func checkTimestampedHMAC(d *DeployConfig, header, sep, tsKey, sigKey, join string, body []byte, now time.Time) error {
	if header == "" {
		return errors.New("missing signature header")
	}
	ts, sigs := signedHeader(header, sep, tsKey, sigKey)
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return errors.New("malformed signature header")
	}
	if skew := now.Sub(time.Unix(n, 0)); skew > d.MaxSkew.Duration || skew < -d.MaxSkew.Duration {
		return fmt.Errorf("timestamp is %s away from the server's clock (max %s)", formatDuration(skew.Abs()), d.MaxSkew)
	}
	mac := hmac.New(sha256.New, d.secret)
	mac.Write([]byte(ts + join))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, s := range sigs {
		if hmac.Equal(s, want) {
			return nil
		}
	}
	return errors.New("invalid signature")
}

func verifyStripe(d *DeployConfig, r *http.Request, body []byte, now time.Time) error {
	return checkTimestampedHMAC(d, r.Header.Get("Stripe-Signature"), ",", "t", "v1", ".", body, now)
}

func verifyPaddle(d *DeployConfig, r *http.Request, body []byte, now time.Time) error {
	return checkTimestampedHMAC(d, r.Header.Get("Paddle-Signature"), ";", "ts", "h1", ":", body, now)
}

func verifyLemonSqueezy(d *DeployConfig, r *http.Request, body []byte, _ time.Time) error {
	got, err := hex.DecodeString(strings.TrimSpace(r.Header.Get("X-Signature")))
	if err != nil || len(got) == 0 {
		return errors.New("missing or malformed X-Signature")
	}
	mac := hmac.New(sha256.New, d.secret)
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return errors.New("invalid signature")
	}
	return nil
}

func jsonString(doc any, path string) string {
	steps, err := parseJSONPath(path)
	if err != nil {
		return ""
	}
	v, ok := lookupJSON(doc, steps)
	if !ok {
		return ""
	}
	s, _ := scalarString(v)
	if !defaultParamMatch.MatchString(s) {
		return ""
	}
	return truncate(s, 128)
}

func stripeEvent(_ *http.Request, doc any) (id, typ, resource string) {
	return jsonString(doc, "id"), jsonString(doc, "type"), jsonString(doc, "data.object.id")
}

func paddleEvent(_ *http.Request, doc any) (id, typ, resource string) {
	return jsonString(doc, "event_id"), jsonString(doc, "event_type"), jsonString(doc, "data.id")
}

func lemonSqueezyEvent(r *http.Request, doc any) (id, typ, resource string) {
	// No event ID: duplicates are told apart by the body hash (see handlePayment).
	typ = firstNonEmpty(jsonString(doc, "meta.event_name"), r.Header.Get("X-Event-Name"))
	return "", typ, jsonString(doc, "data.id")
}

func (s *Server) handlePayment(d *DeployConfig) http.HandlerFunc {
	p := paymentProviders[d.Provider]
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "cannot read body")
			return
		}
		if err := p.verify(d, r, body, time.Now()); err != nil {
			log.Printf("deploy=%s rejected: %s: %v from %s", d.Name, d.Provider, err, s.clientIP(r))
			writeError(w, http.StatusUnauthorized, "invalid signature")
			return
		}
		doc, err := decodeJSON(body)
		if err != nil {
			s.paymentReject(w, d, Trigger{}, "invalid JSON payload")
			return
		}
		id, typ, resource := p.event(r, doc)
		sum := sha256.Sum256(body)
		delivery := firstNonEmpty(id, "sha256-"+hex.EncodeToString(sum[:8]))
		t := Trigger{
			Source:     TriggerWebhook,
			Provider:   d.Provider,
			Delivery:   delivery,
			Repository: d.Repository,
			Pusher:     d.Provider,
			Event:      typ,
			ResourceID: resource,
		}
		if len(d.Events) > 0 && !contains(d.Events, typ) {
			ignore(w, d, delivery, "event "+typ+" not in "+strings.Join(d.Events, ", "))
			return
		}
		params, reason, err := applyRules(d, doc)
		switch {
		case err != nil:
			s.paymentReject(w, d, t, err.Error())
			return
		case reason != "":
			ignore(w, d, delivery, fmt.Sprintf("%s %s: %s", typ, resource, reason))
			return
		}
		t.Params = params
		if d.payloadFile {
			t.Payload = body
		}
		s.submit(w, d, t)
	}
}

// paymentReject answers 200: the platform would retry an error, and the
// same event would fail the same way. It is logged and notified instead.
func (s *Server) paymentReject(w http.ResponseWriter, d *DeployConfig, t Trigger, reason string) {
	log.Printf("deploy=%s delivery=%s rejected %s %s: %s", d.Name, t.Delivery, t.Event, t.ResourceID, reason)
	s.runner.notifyRejected(d, t, reason)
	writeJSON(w, http.StatusOK, map[string]string{"deploy": d.Name, "status": "rejected", "reason": reason})
}

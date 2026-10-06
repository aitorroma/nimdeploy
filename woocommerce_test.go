package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// newWooEnv adds a woocommerce deploy "shop" running script.
func newWooEnv(t *testing.T, script, section, extra string) *env {
	t.Helper()
	return newEnv(t, `echo agency`, extra+`
[deploy.shop]
provider = "woocommerce"
path = "/hooks/shop"
store_url = "https://shop.example.com"
secret_env = "TEST_WEBHOOK_SECRET"
statuses = ["processing", "completed"]
command = "/bin/sh"
args = ["-c", `+jsonQuote(script)+`]
timeout = "5s"
`+section+"\n")
}

func wooSign(body string) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(body))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

type wooOpts struct {
	topic, delivery, source, sig string
	unsigned                     bool
}

func (e *env) woo(t *testing.T, body string, o wooOpts) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/hooks/shop", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if o.topic == "" {
		o.topic = "order.updated"
	}
	if o.delivery == "" {
		o.delivery = "d41d8cd98f00b204e9800998ecf8427e"
	}
	if o.source == "" {
		o.source = "https://shop.example.com/"
	}
	if !o.unsigned {
		req.Header.Set("X-WC-Webhook-ID", "12")
		req.Header.Set("X-WC-Webhook-Topic", o.topic)
		req.Header.Set("X-WC-Webhook-Source", o.source)
		req.Header.Set("X-WC-Webhook-Delivery-ID", o.delivery)
		sig := o.sig
		if sig == "" {
			sig = wooSign(body)
		}
		req.Header.Set("X-WC-Webhook-Signature", sig)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func order(id int, status string) string {
	return fmt.Sprintf(`{"id":%d,"status":%q,"total":"49.00","billing":{"email":"ana@example.com"},"line_items":[{"sku":"pro"}]}`, id, status)
}

func TestWooCommerceWebhook(t *testing.T) {
	e := newWooEnv(t, `echo "event=$DEPLOY_EVENT id=$DEPLOY_RESOURCE_ID plan=$PLAN"; cat "$DEPLOY_PAYLOAD_FILE"; echo; ls -l "$DEPLOY_PAYLOAD_FILE" | cut -c1-10`, `
[deploy.shop.params]
PLAN = { from = "line_items[0].sku", enum = ["basic", "pro"] }
`, "")

	// The ping WooCommerce sends on creation: unsigned form body, must get 200.
	req := httptest.NewRequest(http.MethodPost, "/hooks/shop", strings.NewReader("webhook_id=12"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("ping: %d %s", rec.Code, rec.Body)
	}

	if rec := e.woo(t, order(1, "processing"), wooOpts{unsigned: true}); rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned: %d", rec.Code)
	}
	if rec := e.woo(t, order(1, "processing"), wooOpts{sig: base64.StdEncoding.EncodeToString([]byte("nope"))}); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad signature: %d", rec.Code)
	}
	ignored := map[string]wooOpts{
		"topic":  {topic: "product.updated"},
		"store":  {source: "https://other.example.com/"},
		"status": {},
	}
	for what, o := range ignored {
		body := order(1, "processing")
		if what == "status" {
			body = order(1, "pending")
		}
		if rec := e.woo(t, body, o); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ignored") {
			t.Errorf("%s not ignored: %d %s", what, rec.Code, rec.Body)
		}
	}

	if rec := e.woo(t, order(1234, "processing"), wooOpts{}); rec.Code != http.StatusAccepted {
		t.Fatalf("order: %d %s", rec.Code, rec.Body)
	}
	st := e.waitIdleName(t, "shop")
	if st.Status != StatusSuccess || st.Event != "order.updated" || st.ResourceID != "1234" || st.Repository != "https://shop.example.com" {
		t.Fatalf("state %+v", st)
	}
	out := readLatest(t, e, "shop")
	for _, w := range []string{"event=order.updated id=1234 plan=pro", `"billing":{"email":"ana@example.com"}`, "-rw-------", "resource_id=1234"} {
		if !strings.Contains(out, w) {
			t.Errorf("log missing %q:\n%s", w, out)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(e.logDir, "shop", ".*payload*")); len(left) > 0 {
		t.Errorf("payload file not removed: %v", left)
	}
	if h, _ := e.runner.History("shop", 1); len(h) != 1 || h[0].Event != "order.updated" || h[0].ResourceID != "1234" {
		t.Errorf("history %+v", h)
	}

	// Same delivery ID within a second, different order: both are new.
	if rec := e.woo(t, order(1235, "completed"), wooOpts{}); rec.Code != http.StatusAccepted {
		t.Errorf("second order with the same delivery ID: %d %s", rec.Code, rec.Body)
	}
	e.waitIdleName(t, "shop")
	// Exactly the same delivery again: duplicate.
	if rec := e.woo(t, order(1235, "completed"), wooOpts{}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "duplicate") {
		t.Errorf("duplicate: %d %s", rec.Code, rec.Body)
	}
}

func TestWooCommerceRejectsWith200AndNotifies(t *testing.T) {
	srv, got := recorder(t)
	t.Setenv("TEST_NOTIFY_URL", srv.URL)
	marker := filepath.Join(t.TempDir(), "ran")
	e := newWooEnv(t, `touch `+marker, `
[deploy.shop.params]
PLAN = { from = "line_items[0].sku", enum = ["basic"], required = true }
`, `[notify]
format = "json"
url_env = "TEST_NOTIFY_URL"
`)
	rec := e.woo(t, order(77, "processing"), wooOpts{})
	// Never an error to a signed delivery: WooCommerce would disable the webhook.
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rejected"`) || !strings.Contains(rec.Body.String(), "must be one of basic") {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	if rec := e.woo(t, "{not json", wooOpts{}); rec.Code != http.StatusOK {
		t.Errorf("invalid JSON: %d", rec.Code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(got()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	// Sent concurrently: look for the order's among them.
	msgs := got()
	found := false
	for _, m := range msgs {
		b, _ := json.Marshal(m.body)
		found = found || (strings.Contains(string(b), "rejected, not run") && strings.Contains(string(b), `"resource_id":"77"`))
	}
	if !found {
		t.Fatalf("notifications %+v", msgs)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("rejected order ran")
	}
}

func TestQueueModeAllKeepsEveryRun(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	e := newWooEnv(t, `echo "$DEPLOY_RESOURCE_ID" >> `+runs+`; sleep 0.3`, "", "")
	for i := 1; i <= 4; i++ {
		rec := e.woo(t, order(i, "processing"), wooOpts{delivery: "x" + strconv.Itoa(i)})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("order %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if st := e.runner.State("shop"); st.Queued == nil || st.Queued.Count != 3 {
		t.Fatalf("queued %+v", st.Queued)
	}
	e.waitIdleName(t, "shop")
	b, _ := os.ReadFile(runs)
	if got := strings.Fields(string(b)); strings.Join(got, ",") != "1,2,3,4" {
		t.Fatalf("runs %v, want 1,2,3,4 in order", got)
	}
	if _, err := os.Stat(filepath.Join(e.logDir, "shop", queueFileName)); err == nil {
		t.Error("queue.json left behind")
	}
}

func TestQueueMax(t *testing.T) {
	e := newWooEnv(t, `sleep 0.5`, `queue_max = 1`, "")
	codes := []int{}
	for i := 1; i <= 3; i++ {
		codes = append(codes, e.woo(t, order(i, "processing"), wooOpts{delivery: "q" + strconv.Itoa(i)}).Code)
	}
	if fmt.Sprint(codes) != "[202 202 503]" {
		t.Fatalf("codes %v", codes)
	}
	e.waitIdleName(t, "shop")
}

// A restart keeps queued runs and runs again the one it interrupted.
func TestQueueSurvivesRestart(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	e := newWooEnv(t, `echo "start $DEPLOY_RESOURCE_ID" >> `+runs+`; sleep 1; echo "done $DEPLOY_RESOURCE_ID" >> `+runs, "", "")
	for i := 1; i <= 3; i++ {
		if rec := e.woo(t, order(i, "processing"), wooOpts{delivery: "r" + strconv.Itoa(i)}); rec.Code != http.StatusAccepted {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	}
	time.Sleep(200 * time.Millisecond)
	e.runner.Shutdown(100 * time.Millisecond) // kills order 1 mid-run

	b, err := os.ReadFile(filepath.Join(e.logDir, "shop", queueFileName))
	if err != nil {
		t.Fatalf("queue.json: %v", err)
	}
	var q queueFile
	if err := json.Unmarshal(b, &q); err != nil || len(q.Interrupted) != 1 || len(q.Pending) != 2 || q.Interrupted[0].ResourceID != "1" {
		t.Fatalf("queue.json %s (%v)", b, err)
	}
	if info, _ := os.Stat(filepath.Join(e.logDir, "shop", queueFileName)); info.Mode().Perm() != 0o600 {
		t.Errorf("queue.json mode %v", info.Mode().Perm())
	}

	runner2, err := NewRunner(e.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runner2.Shutdown(5 * time.Second) })
	e.runner = runner2
	e.waitIdleName(t, "shop")
	out, _ := os.ReadFile(runs)
	for _, want := range []string{"done 1", "done 2", "done 3"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Count(string(out), "start 1") != 2 {
		t.Errorf("order 1 should have started twice:\n%s", out)
	}
}

// --- CLI against a fake shop ----------------------------------------------------------------

type fakeShop struct {
	mu       sync.Mutex
	webhooks []wooWebhook
	nextID   int
	notes    []string
	statuses map[string]string
	orders   map[string]string
}

func (f *fakeShop) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "ck_test" || p != "cs_test" {
			http.Error(w, `{"code":"woocommerce_rest_cannot_view","message":"Sorry, you cannot list resources."}`, 401)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/wp-json/wc/v3")
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case path == "/webhooks" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(f.webhooks)
		case path == "/webhooks" && r.Method == http.MethodPost:
			var wh wooWebhook
			_ = json.Unmarshal(body, &wh)
			f.nextID++
			wh.ID = f.nextID
			f.webhooks = append(f.webhooks, wh)
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(wh)
		case strings.HasPrefix(path, "/webhooks/") && r.Method == http.MethodPut:
			id, _ := strconv.Atoi(strings.TrimPrefix(path, "/webhooks/"))
			var upd wooWebhook
			_ = json.Unmarshal(body, &upd)
			for i := range f.webhooks {
				if f.webhooks[i].ID == id {
					if upd.Status != "" {
						f.webhooks[i].Status = upd.Status
					}
					if upd.Secret != "" {
						f.webhooks[i].Secret = upd.Secret
					}
					_ = json.NewEncoder(w).Encode(f.webhooks[i])
				}
			}
		case path == "/orders" && r.Method == http.MethodGet:
			var list []json.RawMessage
			for _, id := range sortedKeys(f.orders) {
				if st := r.URL.Query().Get("status"); st == "" || strings.Contains(f.orders[id], `"status":"`+st+`"`) {
					list = append(list, json.RawMessage(f.orders[id]))
				}
			}
			_ = json.NewEncoder(w).Encode(list)
		case strings.HasSuffix(path, "/notes") && r.Method == http.MethodPost:
			f.notes = append(f.notes, string(body))
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasPrefix(path, "/orders/") && r.Method == http.MethodGet:
			o, ok := f.orders[strings.TrimPrefix(path, "/orders/")]
			if !ok {
				http.Error(w, `{"code":"woocommerce_rest_shop_order_invalid_id","message":"Invalid ID."}`, 404)
				return
			}
			_, _ = w.Write([]byte(o))
		case strings.HasPrefix(path, "/orders/") && r.Method == http.MethodPut:
			f.statuses[strings.TrimPrefix(path, "/orders/")] = string(body)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "unexpected "+r.Method+" "+path, 400)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWooCommerceCLI(t *testing.T) {
	shop := &fakeShop{statuses: map[string]string{}, orders: map[string]string{
		"10": order(10, "processing"), "11": order(11, "processing"), "12": order(12, "pending"),
	}}
	shopSrv := shop.serve(t)

	// A user install's config, then "woocommerce add".
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	configPath := filepath.Join(dir, "config.toml")
	envFile := filepath.Join(dir, "secrets.env")
	_ = os.WriteFile(configPath, []byte(`[server]
listen = "127.0.0.1:0"
api_token_env = "NIMDEPLOY_API_TOKEN"
[logging]
directory = "`+logDir+`"
[deploy.site]
path = "/hooks/site"
repository = "acme/site"
secret_env = "SITE_WEBHOOK_SECRET"
command = "/bin/true"
`), 0o640)
	_ = os.WriteFile(envFile, []byte("NIMDEPLOY_API_TOKEN="+testToken+"\nSITE_WEBHOOK_SECRET=abc\n"), 0o600)
	t.Setenv("WC_CONSUMER_KEY", "ck_test")
	t.Setenv("WC_CONSUMER_SECRET", "cs_test")
	for _, k := range []string{"NIMDEPLOY_API_TOKEN", "SITE_WEBHOOK_SECRET", "ORDERS_WEBHOOK_SECRET", "ORDERS_WC_KEY", "ORDERS_WC_SECRET"} {
		t.Setenv(k, "")
	}
	runs := filepath.Join(dir, "runs")
	code := wooAdd(configPath, envFile, []string{"-store", shopSrv.URL, "-url", "http://127.0.0.1:8080", "-name", "orders", "-dir", dir,
		"-command", `echo "$DEPLOY_RESOURCE_ID" >> ` + runs})
	if code != 0 {
		t.Fatalf("add exit %d", code)
	}
	cfgText, _ := os.ReadFile(configPath)
	for _, w := range []string{`[deploy.orders]`, `provider = "woocommerce"`, `webhook_url = "http://127.0.0.1:8080/hooks/orders"`, `statuses = ["processing", "completed"]`} {
		if !strings.Contains(string(cfgText), w) {
			t.Errorf("config missing %q:\n%s", w, cfgText)
		}
	}
	secrets, _ := os.ReadFile(envFile)
	if !strings.Contains(string(secrets), "ORDERS_WC_KEY=ck_test") || !strings.Contains(string(secrets), "ORDERS_WEBHOOK_SECRET=") {
		t.Errorf("secrets:\n%s", secrets)
	}
	if info, _ := os.Stat(envFile); info.Mode().Perm() != 0o600 {
		t.Errorf("secrets mode %v", info.Mode().Perm())
	}
	if len(shop.webhooks) != 2 || shop.webhooks[0].Topic != "order.created" || shop.webhooks[1].Status != "active" {
		t.Fatalf("webhooks %+v", shop.webhooks)
	}
	secret := shop.webhooks[0].Secret
	if !strings.Contains(string(secrets), "ORDERS_WEBHOOK_SECRET="+secret) {
		t.Errorf("the shop got another secret than secrets.env has")
	}

	// The service with that config.
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := loadSecretsInto(envFile); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ResolveSecrets(); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runner.Shutdown(5 * time.Second) })
	srv := httptest.NewServer(NewServer(cfg, runner).Routes())
	defer srv.Close()
	cfg.Server.Listen = strings.TrimPrefix(srv.URL, "http://")

	// register again: idempotent.
	if code := cliWooCommerce(cfg, configPath, envFile, []string{"register", "orders"}); code != 0 || len(shop.webhooks) != 2 {
		t.Fatalf("re-register exit %d, %d webhooks", code, len(shop.webhooks))
	}
	// status: a disabled one is reported, -enable fixes it.
	shop.webhooks[1].Status = "disabled"
	if code := cliWooCommerce(cfg, configPath, envFile, []string{"status", "orders"}); code != 1 {
		t.Errorf("status with a disabled webhook exit %d", code)
	}
	if code := cliWooCommerce(cfg, configPath, envFile, []string{"status", "orders", "-enable"}); code != 0 || shop.webhooks[1].Status != "active" {
		t.Errorf("status -enable exit %d, status %s", code, shop.webhooks[1].Status)
	}

	// replay by ID and by status: pending order 12 is ignored by statuses.
	if code := cliWooCommerce(cfg, configPath, envFile, []string{"replay", "orders", "10", "12"}); code != 0 {
		t.Fatalf("replay exit %d", code)
	}
	waitRuns := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			b, _ := os.ReadFile(runs)
			if strings.Join(strings.Fields(string(b)), ",") == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		b, _ := os.ReadFile(runs)
		t.Fatalf("runs %q, want %s", b, want)
	}
	waitRuns("10")
	if code := cliWooCommerce(cfg, configPath, envFile, []string{"replay", "orders", "-status", "processing"}); code != 0 {
		t.Fatalf("replay -status exit %d", code)
	}
	waitRuns("10,10,11")
	if st := runner.State("orders"); st.Trigger != TriggerManual || st.Event != "order.updated" {
		t.Errorf("replayed state %+v", st)
	}

	// note: what a script calls to close the loop.
	if code := cliWooCommerce(cfg, configPath, envFile, []string{"note", "orders", "-status", "completed", "-customer", "10", "Your site is ready"}); code != 0 {
		t.Fatalf("note exit %d", code)
	}
	if len(shop.notes) != 1 || !strings.Contains(shop.notes[0], `"customer_note":true`) || !strings.Contains(shop.statuses["10"], `"completed"`) {
		t.Errorf("notes %v statuses %v", shop.notes, shop.statuses)
	}

	// Errors: unknown deploy, wrong keys.
	if code := cliWooCommerce(cfg, configPath, envFile, []string{"status", "site"}); code != 2 {
		t.Errorf("non-woocommerce deploy exit %d", code)
	}
	cfg.Deploy["orders"].apiSecret = "wrong"
	t.Setenv("ORDERS_WC_SECRET", "wrong")
	if code := cliWooCommerce(cfg, configPath, envFile, []string{"status", "orders"}); code != 1 {
		t.Errorf("wrong keys exit %d", code)
	}
}

func TestWooCommerceConfig(t *testing.T) {
	base := "[logging]\ndirectory = \"/tmp/x\"\n[deploy.s]\npath = \"/hooks/s\"\nsecret_env = \"S\"\ncommand = \"/bin/true\"\n"
	cases := map[string]string{
		"provider = \"woocommerce\"\nbranch = \"main\"":                                   "branch does not apply",
		"provider = \"woocommerce\"\ntopics = [\"order.paid\"]":                           "topic \"order.paid\"",
		"provider = \"woocommerce\"\nstatuses = [\"wc-Processing\"]":                      "status \"wc-Processing\"",
		"provider = \"woocommerce\"\nstore_url = \"shop.example.com\"":                    "store_url must be",
		"provider = \"woocommerce\"\nwebhook_url = \"http://deploy.example.com/hooks/s\"": "webhook_url must be https",
		"provider = \"woocommerce\"\nstore_url = \"https://a\"\napi_key_env = \"K\"":      "go together",
		"provider = \"woocommerce\"\nqueue_mode = \"fifo\"":                               "queue_mode must be",
		"provider = \"woocommerce\"\nqueue = false":                                       "needs queue = true",
		"repository = \"a/b\"\ntopics = [\"order.created\"]":                              "topics is only for provider = \"woocommerce\"",
	}
	for extra, want := range cases {
		path := filepath.Join(t.TempDir(), "c.toml")
		_ = os.WriteFile(path, []byte(base+extra+"\n"), 0o600)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", extra, err, want)
		}
	}
	path := filepath.Join(t.TempDir(), "c.toml")
	_ = os.WriteFile(path, []byte(base+"provider = \"woocommerce\"\nstore_url = \"https://shop.example.com/\"\n"), 0o600)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Deploy["s"]
	if !d.queueAll || !d.payloadFile || d.QueueMax != defaultQueueMax || fmt.Sprint(d.Topics) != "[order.created order.updated]" || d.Repository != "https://shop.example.com" {
		t.Fatalf("defaults %+v", d)
	}
}

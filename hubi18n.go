package main

import (
	"fmt"
	"net/http"
	"strings"
)

// The hub's dashboard speaks English and Spanish: the "lang" cookie when set
// (the selector in the sidebar), else the browser's Accept-Language.

var hubLangs = []string{"en", "es"}

var hubText = map[string]map[string]string{
	"en": {
		"nav.overview": "Overview", "nav.activity": "Activity", "nav.attention": "Needs attention", "nav.agents": "Agents",
		"side.clients": "Clients", "side.from_label": "from the client label", "side.no_match": "No client matches",
		"top.search": "Search apps, clients, agents", "top.live": "Live", "top.add_agent": "Add agent", "top.theme": "Theme", "top.logout": "Log out",
		"ov.title": "Overview", "ov.sub": "%d apps across %d clients on %d servers.", "ov.all_envs": "All environments",
		"kpi.deploys7": "Deploys · 7 days", "kpi.vs_prev": "%s vs previous week", "kpi.failing": "Failing now", "kpi.failed_week": "%d failed runs this week",
		"kpi.success": "Success rate · 7 days", "kpi.median": "median duration %s", "kpi.online": "Servers online", "kpi.offline_pending": "%d offline · %d not connected",
		"chart.title": "Deploys per day", "chart.sub": "last 14 days, all servers", "chart.success": "success", "chart.failed": "failed", "chart.other": "skipped", "chart.today": "today",
		"mx.title": "Clients and environments", "mx.sub": "dashed: its server is offline", "mx.client": "Client", "mx.apps": "%d apps", "mx.failing": "%d failing",
		"attn.title": "Needs attention", "attn.clear": "Everything is green.", "attn.sub": "Failing apps, servers that stopped reporting and outdated agents, most urgent first.",
		"attn.failed": "%s failed", "attn.offline": "%s is offline", "attn.offline_d": "%s · last heartbeat %s ago · %d events waiting",
		"attn.old": "%s runs %s", "attn.old_d": "Older than the hub (%s): update it with the installer",
		"attn.pending": "%s never connected", "attn.pending_d": "%s · created, waiting for its first heartbeat", "no_client": "No client",
		"attn.cert": "%s's certificate expires %s", "attn.cert_d": "Issue a new one from Agents before it stops connecting",
		"act.title": "Activity", "act.all": "All activity", "act.sub": "Every deploy, refused request and server change, newest first.",
		"ev.started": "started", "ev.finished": "finished", "ev.rejected": "rejected", "ev.command": "command",
		"cl.apps_failing": "%d apps · %d failing", "cl.add_server": "Add server", "cl.no_apps": "No apps yet. They appear with this server's first heartbeat, taken from its config.",
		"cl.waiting_first": "waiting for its first connection", "cl.seen": "seen %s ago", "cl.offline_for": "offline for %s",
		"app.no_runs": "no runs yet", "app.ago": "%s ago", "app.last_runs": "last runs, oldest first",
		"dp.last_run": "Last run", "dp.commit": "Commit", "dp.source": "Source", "dp.by": "Triggered by", "dp.never": "never", "dp.scheduled": "scheduled",
		"dp.error": "Error", "dp.phases": "Phases", "dp.phases_sub": "from the deploy's trace", "dp.hosts": "Hosts", "dp.hosts_sub": "%d hosts · %d changed · %d failed",
		"dp.log": "Log", "dp.log_sub": "last lines sent by the server", "dp.runs": "Runs", "dp.labels": "Labels", "dp.copy_cmd": "Copy run command",
		"dp.run_again": "Run again", "dp.rollback": "Roll back", "dp.confirm_run": "Run %s again on %s?", "dp.confirm_rb": "Roll %s back to the last good commit on %s?",
		"dp.confirm": "Confirm", "dp.cancel": "Cancel", "dp.commands": "Commands from the hub", "dp.no_commands_agent": "%s doesn't accept commands: set accept_commands = true in its [hub] section.",
		"cmd.pending": "waiting for the server", "cmd.sent": "sent", "cmd.running": "running", "cmd.done": "done", "cmd.refused": "refused", "cmd.expired": "expired",
		"ag.title": "Agents", "ag.sub": "Each server runs nimdeploy and sends its deploys here. The hub never connects to them.", "ag.none": "No agents yet. Add the first one: it takes a token and four lines of config.",
		"ag.beats": "Heartbeats · last 30 min", "ag.host": "Host", "ag.version": "Version", "ag.last_seen": "Last seen", "ag.up_since": "Up since", "ag.waiting": "Waiting",
		"ag.events_to_send": "%d events to send", "ag.nothing": "nothing", "ag.update": "update", "ag.cert": "Certificate", "ag.cert_until": "valid until %s", "ag.no_cert": "not required",
		"ag.commands": "Commands", "ag.commands_on": "accepted", "ag.commands_off": "off",
		"ag.pending_note": "Created, waiting for its first heartbeat. Its token was shown once; create a new one if it was lost.",
		"ag.new_token":    "New token", "ag.revoke": "Revoke", "ag.confirm_revoke": "Confirm revoke", "ag.revoked": "revoked",
		"st.online": "online", "st.offline": "offline", "st.pending": "not connected", "st.revoked": "revoked",
		"st.success": "success", "st.failed": "failed", "st.running": "running", "st.waiting": "waiting", "st.queued": "queued", "st.skipped": "skipped", "st.interrupted": "interrupted", "st.never": "never run",
		"new.title": "Add agent", "new.intro": "An agent is one server running nimdeploy. It gets its own token and sends its deploys here; the hub never connects to it.",
		"new.client": "Client", "new.client_hint": "Pick one or type a new name: it becomes the server's client label.", "new.env": "Environment", "new.name": "Agent name", "new.name_hint": "Letters, digits, _ . -",
		"new.cert": "Require a client certificate", "new.cert_d": "The hub issues one named like the agent; events without it are refused. Recommended for production.",
		"new.cert_off": "Needs the hub to terminate TLS with its agent CA (-agent-ca-dir).", "new.create": "Create agent", "new.cancel": "Cancel", "new.close": "Close", "new.done": "Done",
		"new.step1": "Server", "new.step2": "Connect", "new.step3": "Verify", "new.connect": "Connect %s",
		"new.token": "Token", "new.copy": "Copy", "new.once": "Shown once.", "new.once_d": "Store it in the server's secrets.env. If it gets lost, create a new token from Agents.",
		"new.certfiles": "Client certificate", "new.save_cert": "Save certificate", "new.save_key": "Save key",
		"new.s1": "Install or update nimdeploy on the server", "new.s2": "Add to its config.toml", "new.s3": "Add the token to secrets.env and restart",
		"new.wait": "Waiting for %s's first heartbeat. This updates by itself.", "new.connected": "%s connected: %s, %d apps reported.", "new.open": "Open %s",
		"login.title": "Sign in", "login.hint": "The hub token ($NIMDEPLOY_HUB_TOKEN).", "login.wrong": "Wrong token", "login.button": "Sign in",
		"err.db": "The database is not answering: %s", "err.notfound": "No deploy %s on %s",
		"lang.name":           "English",
		"nav.notify":          "Notifications",
		"nf.title":            "Notifications",
		"nf.sub":              "Rules choose which events go where. A rule can cover several clients, environments, servers and deploys, and send to several destinations.",
		"nf.rules":            "Rules",
		"nf.rules_sub":        "what to tell, about whom",
		"nf.new_rule":         "New rule",
		"nf.no_rules":         "No rules yet. Create a destination (a Slack channel, a Telegram chat, an email…), then a rule that sends to it.",
		"nf.channels":         "Destinations",
		"nf.channels_sub":     "where messages go",
		"nf.new_channel":      "New destination",
		"nf.no_channels":      "No destinations yet.",
		"nf.on":               "active",
		"nf.off":              "paused",
		"nf.pause":            "Pause",
		"nf.resume":           "Resume",
		"nf.edit":             "Edit",
		"nf.delete":           "Delete",
		"nf.test":             "Send test",
		"nf.confirm_rule":     "Delete the rule %s?",
		"nf.confirm_channel":  "Delete the destination %s? Rules that use it stop sending there.",
		"nf.filters_all":      "Every client, environment and server",
		"nf.f_clients":        "Clients",
		"nf.f_envs":           "Environments",
		"nf.f_agents":         "Servers",
		"nf.f_deploys":        "Deploys",
		"nf.no_dests":         "no destination",
		"nf.never":            "not used yet",
		"nf.last_ok":          "sent %s ago",
		"nf.last_err":         "failed %s ago: %s",
		"nf.no_public_url":    "Messages carry no link to the hub until it knows its address: set -public-url (NIMDEPLOY_HUB_PUBLIC_URL).",
		"nf.rule_new":         "New rule",
		"nf.rule_edit":        "Edit rule",
		"nf.name":             "Name",
		"nf.name_ph":          "Production failures",
		"nf.events":           "Events",
		"nf.filters":          "Only for",
		"nf.filters_hint":     "Tick nothing in a group to include all of it. Server events (offline, certificate) ignore the deploy filter.",
		"nf.dests":            "Send to",
		"nf.dests_none":       "Create a destination first: the rule needs somewhere to send.",
		"nf.lang":             "Message language",
		"nf.enabled":          "Active",
		"nf.save":             "Save",
		"nf.err_name":         "Give it a name (up to 100 characters).",
		"nf.err_events":       "Pick at least one event.",
		"nf.err_dests":        "Pick at least one destination.",
		"nf.ch_new":           "New destination",
		"nf.ch_edit":          "Edit destination",
		"nf.ch_name_ph":       "Slack #deploys",
		"nf.kind":             "Type",
		"nf.url_hint":         "Slack or Discord incoming webhook, or any URL that accepts a JSON POST.",
		"nf.keep":             "unchanged: leave empty to keep it",
		"nf.bot_token":        "Bot token",
		"nf.chat_id":          "Chat id",
		"nf.smtp_host":        "SMTP server",
		"nf.smtp_port":        "Port",
		"nf.smtp_tls":         "Security",
		"nf.smtp_none":        "None (local relay only)",
		"nf.smtp_user":        "User",
		"nf.smtp_pass":        "Password",
		"nf.from":             "From",
		"nf.to":               "To (comma-separated)",
		"nf.secrets_note":     "URLs, tokens and passwords are kept in the hub's database and never shown again.",
		"nf.test_ok":          "Sent ✓",
		"nf.test_fail":        "Failed: %s",
		"ev.deploy.failed":    "Deploy failed",
		"ev.deploy.recovered": "Deploy recovered",
		"ev.deploy.success":   "Deploy succeeded",
		"ev.deploy.rejected":  "Request rejected",
		"ev.agent.offline":    "Server offline",
		"ev.agent.back":       "Server back online",
		"ev.cert.expiring":    "Certificate expiring",
		"ev.command.refused":  "Command refused",
		"kind.slack":          "Slack",
		"kind.discord":        "Discord",
		"kind.telegram":       "Telegram",
		"kind.webhook":        "Webhook (JSON)",
		"kind.email":          "Email",
		"nt.deploy.failed":    "❌ %s failed",
		"nt.deploy.recovered": "✅ %s recovered",
		"nt.deploy.success":   "✅ %s deployed",
		"nt.deploy.rejected":  "⚠️ %s: request rejected",
		"nt.agent.offline":    "🔴 %s is offline",
		"nt.agent.back":       "🟢 %s is back online",
		"nt.cert.expiring":    "🔐 %s: its certificate expires on %s",
		"nt.command.refused":  "⚠️ %s: the server refused a command",
		"nt.by":               "by %s (%s)",
		"nt.duration":         "duration %s",
		"nt.error":            "error: %s",
		"nt.reason":           "reason: %s",
		"nt.last_seen":        "last seen %s ago",
		"nt.test":             "🔔 Test notification from the nimdeploy hub",
	},
	"es": {
		"nav.overview": "Resumen", "nav.activity": "Actividad", "nav.attention": "Requiere atención", "nav.agents": "Agentes",
		"side.clients": "Clientes", "side.from_label": "según la etiqueta client", "side.no_match": "Ningún cliente coincide",
		"top.search": "Buscar apps, clientes, agentes", "top.live": "En directo", "top.add_agent": "Añadir agente", "top.theme": "Tema", "top.logout": "Salir",
		"ov.title": "Resumen", "ov.sub": "%d apps de %d clientes en %d servidores.", "ov.all_envs": "Todos los entornos",
		"kpi.deploys7": "Despliegues · 7 días", "kpi.vs_prev": "%s respecto a la semana anterior", "kpi.failing": "Fallando ahora", "kpi.failed_week": "%d ejecuciones fallidas esta semana",
		"kpi.success": "Tasa de éxito · 7 días", "kpi.median": "duración mediana %s", "kpi.online": "Servidores conectados", "kpi.offline_pending": "%d caídos · %d sin conectar",
		"chart.title": "Despliegues por día", "chart.sub": "últimos 14 días, todos los servidores", "chart.success": "correctos", "chart.failed": "fallidos", "chart.other": "omitidos", "chart.today": "hoy",
		"mx.title": "Clientes y entornos", "mx.sub": "discontinuo: su servidor no responde", "mx.client": "Cliente", "mx.apps": "%d apps", "mx.failing": "%d fallando",
		"attn.title": "Requiere atención", "attn.clear": "Todo en verde.", "attn.sub": "Apps fallando, servidores que han dejado de informar y agentes desactualizados, lo más urgente primero.",
		"attn.failed": "%s ha fallado", "attn.offline": "%s no responde", "attn.offline_d": "%s · último latido hace %s · %d eventos pendientes",
		"attn.old": "%s usa la %s", "attn.old_d": "Más antigua que el hub (%s): actualízalo con el instalador",
		"attn.pending": "%s nunca se ha conectado", "attn.pending_d": "%s · creado, esperando su primer latido", "no_client": "Sin cliente",
		"attn.cert": "El certificado de %s caduca %s", "attn.cert_d": "Emite uno nuevo desde Agentes antes de que deje de conectar",
		"act.title": "Actividad", "act.all": "Toda la actividad", "act.sub": "Cada despliegue, petición rechazada y cambio de servidor, lo más reciente primero.",
		"ev.started": "empezó", "ev.finished": "terminó", "ev.rejected": "rechazado", "ev.command": "orden",
		"cl.apps_failing": "%d apps · %d fallando", "cl.add_server": "Añadir servidor", "cl.no_apps": "Aún no hay apps. Aparecen con el primer latido de este servidor, sacadas de su configuración.",
		"cl.waiting_first": "esperando su primera conexión", "cl.seen": "visto hace %s", "cl.offline_for": "sin responder desde hace %s",
		"app.no_runs": "sin ejecuciones", "app.ago": "hace %s", "app.last_runs": "últimas ejecuciones, de la más antigua a la más reciente",
		"dp.last_run": "Última ejecución", "dp.commit": "Commit", "dp.source": "Origen", "dp.by": "Lanzado por", "dp.never": "nunca", "dp.scheduled": "programado",
		"dp.error": "Error", "dp.phases": "Fases", "dp.phases_sub": "según la traza del despliegue", "dp.hosts": "Hosts", "dp.hosts_sub": "%d hosts · %d con cambios · %d fallidos",
		"dp.log": "Log", "dp.log_sub": "últimas líneas enviadas por el servidor", "dp.runs": "Ejecuciones", "dp.labels": "Etiquetas", "dp.copy_cmd": "Copiar comando",
		"dp.run_again": "Volver a lanzar", "dp.rollback": "Volver atrás", "dp.confirm_run": "¿Volver a lanzar %s en %s?", "dp.confirm_rb": "¿Volver %s al último commit correcto en %s?",
		"dp.confirm": "Confirmar", "dp.cancel": "Cancelar", "dp.commands": "Órdenes desde el hub", "dp.no_commands_agent": "%s no acepta órdenes: pon accept_commands = true en su sección [hub].",
		"cmd.pending": "esperando al servidor", "cmd.sent": "enviada", "cmd.running": "en curso", "cmd.done": "hecha", "cmd.refused": "rechazada", "cmd.expired": "caducada",
		"ag.title": "Agentes", "ag.sub": "Cada servidor ejecuta nimdeploy y envía aquí sus despliegues. El hub nunca se conecta a ellos.", "ag.none": "Aún no hay agentes. Añade el primero: solo hace falta un token y cuatro líneas de configuración.",
		"ag.beats": "Latidos · últimos 30 min", "ag.host": "Host", "ag.version": "Versión", "ag.last_seen": "Último contacto", "ag.up_since": "En marcha desde", "ag.waiting": "Pendiente",
		"ag.events_to_send": "%d eventos por enviar", "ag.nothing": "nada", "ag.update": "actualizar", "ag.cert": "Certificado", "ag.cert_until": "válido hasta %s", "ag.no_cert": "no requerido",
		"ag.commands": "Órdenes", "ag.commands_on": "aceptadas", "ag.commands_off": "desactivadas",
		"ag.pending_note": "Creado, esperando su primer latido. Su token se mostró una sola vez; crea uno nuevo si se perdió.",
		"ag.new_token":    "Nuevo token", "ag.revoke": "Revocar", "ag.confirm_revoke": "Confirmar", "ag.revoked": "revocado",
		"st.online": "conectado", "st.offline": "sin respuesta", "st.pending": "sin conectar", "st.revoked": "revocado",
		"st.success": "correcto", "st.failed": "fallido", "st.running": "en curso", "st.waiting": "esperando", "st.queued": "en cola", "st.skipped": "omitido", "st.interrupted": "interrumpido", "st.never": "sin ejecutar",
		"new.title": "Añadir agente", "new.intro": "Un agente es un servidor con nimdeploy. Tiene su propio token y envía aquí sus despliegues; el hub nunca se conecta a él.",
		"new.client": "Cliente", "new.client_hint": "Elige uno o escribe uno nuevo: será la etiqueta client del servidor.", "new.env": "Entorno", "new.name": "Nombre del agente", "new.name_hint": "Letras, números, _ . -",
		"new.cert": "Exigir certificado de cliente", "new.cert_d": "El hub emite uno con el nombre del agente y rechaza sus eventos sin él. Recomendado en producción.",
		"new.cert_off": "Requiere que el hub termine el TLS con su CA de agentes (-agent-ca-dir).", "new.create": "Crear agente", "new.cancel": "Cancelar", "new.close": "Cerrar", "new.done": "Hecho",
		"new.step1": "Servidor", "new.step2": "Conectar", "new.step3": "Comprobar", "new.connect": "Conectar %s",
		"new.token": "Token", "new.copy": "Copiar", "new.once": "Solo se muestra una vez.", "new.once_d": "Guárdalo en el secrets.env del servidor. Si se pierde, crea un token nuevo desde Agentes.",
		"new.certfiles": "Certificado de cliente", "new.save_cert": "Guardar certificado", "new.save_key": "Guardar clave",
		"new.s1": "Instala o actualiza nimdeploy en el servidor", "new.s2": "Añade a su config.toml", "new.s3": "Añade el token a secrets.env y reinicia",
		"new.wait": "Esperando el primer latido de %s. Se actualiza solo.", "new.connected": "%s conectado: %s, %d apps.", "new.open": "Abrir %s",
		"login.title": "Entrar", "login.hint": "El token del hub ($NIMDEPLOY_HUB_TOKEN).", "login.wrong": "Token incorrecto", "login.button": "Entrar",
		"err.db": "La base de datos no responde: %s", "err.notfound": "No hay ningún despliegue %s en %s",
		"lang.name":           "Español",
		"nav.notify":          "Notificaciones",
		"nf.title":            "Notificaciones",
		"nf.sub":              "Las reglas deciden qué eventos van a dónde. Una regla puede cubrir varios clientes, entornos, servidores y despliegues, y enviar a varios destinos.",
		"nf.rules":            "Reglas",
		"nf.rules_sub":        "qué avisar y de quién",
		"nf.new_rule":         "Nueva regla",
		"nf.no_rules":         "Aún no hay reglas. Crea un destino (un canal de Slack, un chat de Telegram, un email…) y luego una regla que envíe a él.",
		"nf.channels":         "Destinos",
		"nf.channels_sub":     "a dónde van los mensajes",
		"nf.new_channel":      "Nuevo destino",
		"nf.no_channels":      "Aún no hay destinos.",
		"nf.on":               "activa",
		"nf.off":              "en pausa",
		"nf.pause":            "Pausar",
		"nf.resume":           "Reanudar",
		"nf.edit":             "Editar",
		"nf.delete":           "Eliminar",
		"nf.test":             "Enviar prueba",
		"nf.confirm_rule":     "¿Eliminar la regla %s?",
		"nf.confirm_channel":  "¿Eliminar el destino %s? Las reglas que lo usan dejarán de enviar ahí.",
		"nf.filters_all":      "Todos los clientes, entornos y servidores",
		"nf.f_clients":        "Clientes",
		"nf.f_envs":           "Entornos",
		"nf.f_agents":         "Servidores",
		"nf.f_deploys":        "Despliegues",
		"nf.no_dests":         "sin destino",
		"nf.never":            "sin usar todavía",
		"nf.last_ok":          "enviado hace %s",
		"nf.last_err":         "falló hace %s: %s",
		"nf.no_public_url":    "Los mensajes no llevan enlace al hub hasta que sepa su dirección: configura -public-url (NIMDEPLOY_HUB_PUBLIC_URL).",
		"nf.rule_new":         "Nueva regla",
		"nf.rule_edit":        "Editar regla",
		"nf.name":             "Nombre",
		"nf.name_ph":          "Fallos en producción",
		"nf.events":           "Eventos",
		"nf.filters":          "Solo para",
		"nf.filters_hint":     "Si no marcas nada en un grupo, se incluye entero. Los eventos de servidor (caído, certificado) no tienen en cuenta el filtro de despliegues.",
		"nf.dests":            "Enviar a",
		"nf.dests_none":       "Crea antes un destino: la regla necesita a dónde enviar.",
		"nf.lang":             "Idioma de los mensajes",
		"nf.enabled":          "Activa",
		"nf.save":             "Guardar",
		"nf.err_name":         "Ponle un nombre (hasta 100 caracteres).",
		"nf.err_events":       "Elige al menos un evento.",
		"nf.err_dests":        "Elige al menos un destino.",
		"nf.ch_new":           "Nuevo destino",
		"nf.ch_edit":          "Editar destino",
		"nf.ch_name_ph":       "Slack #deploys",
		"nf.kind":             "Tipo",
		"nf.url_hint":         "Webhook entrante de Slack o Discord, o cualquier URL que acepte un POST JSON.",
		"nf.keep":             "sin cambios: déjalo vacío para conservarlo",
		"nf.bot_token":        "Token del bot",
		"nf.chat_id":          "Id del chat",
		"nf.smtp_host":        "Servidor SMTP",
		"nf.smtp_port":        "Puerto",
		"nf.smtp_tls":         "Seguridad",
		"nf.smtp_none":        "Ninguna (solo relay local)",
		"nf.smtp_user":        "Usuario",
		"nf.smtp_pass":        "Contraseña",
		"nf.from":             "Remitente",
		"nf.to":               "Para (separados por comas)",
		"nf.secrets_note":     "Las URLs, tokens y contraseñas se guardan en la base de datos del hub y no se vuelven a mostrar.",
		"nf.test_ok":          "Enviado ✓",
		"nf.test_fail":        "Ha fallado: %s",
		"ev.deploy.failed":    "Despliegue fallido",
		"ev.deploy.recovered": "Despliegue recuperado",
		"ev.deploy.success":   "Despliegue correcto",
		"ev.deploy.rejected":  "Petición rechazada",
		"ev.agent.offline":    "Servidor caído",
		"ev.agent.back":       "Servidor de vuelta",
		"ev.cert.expiring":    "Certificado a punto de caducar",
		"ev.command.refused":  "Orden rechazada",
		"kind.slack":          "Slack",
		"kind.discord":        "Discord",
		"kind.telegram":       "Telegram",
		"kind.webhook":        "Webhook (JSON)",
		"kind.email":          "Email",
		"nt.deploy.failed":    "❌ %s ha fallado",
		"nt.deploy.recovered": "✅ %s se ha recuperado",
		"nt.deploy.success":   "✅ %s desplegado",
		"nt.deploy.rejected":  "⚠️ %s: petición rechazada",
		"nt.agent.offline":    "🔴 %s no responde",
		"nt.agent.back":       "🟢 %s vuelve a estar conectado",
		"nt.cert.expiring":    "🔐 %s: su certificado caduca el %s",
		"nt.command.refused":  "⚠️ %s: el servidor rechazó una orden",
		"nt.by":               "por %s (%s)",
		"nt.duration":         "duración %s",
		"nt.error":            "error: %s",
		"nt.reason":           "motivo: %s",
		"nt.last_seen":        "visto por última vez hace %s",
		"nt.test":             "🔔 Notificación de prueba del hub de nimdeploy",
	},
}

// hubLang picks the dashboard's language for a request.
func hubLang(r *http.Request) string {
	if c, err := r.Cookie("lang"); err == nil {
		for _, l := range hubLangs {
			if c.Value == l {
				return l
			}
		}
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		for _, l := range hubLangs {
			if tag == l || strings.HasPrefix(tag, l+"-") {
				return l
			}
		}
	}
	return "en"
}

// hubT translates a key, formatting args; unknown keys fall back to English, then the key.
func hubT(lang, key string, args ...any) string {
	s, ok := hubText[lang][key]
	if !ok {
		if s, ok = hubText["en"][key]; !ok {
			s = key
		}
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

// agoIn renders a duration in the dashboard's language ("3m", "2h", "5d").
func agoIn(lang string, d interface{ Minutes() float64 }) string {
	m := d.Minutes()
	switch {
	case m < 1:
		if lang == "es" {
			return "un momento"
		}
		return "a moment"
	case m < 60:
		return fmt.Sprintf("%dm", int(m))
	case m < 60*24:
		return fmt.Sprintf("%dh", int(m/60))
	}
	return fmt.Sprintf("%dd", int(m/1440))
}

// agoShort is agoIn for narrow columns, where "a moment" would wrap.
func agoShort(lang string, d interface{ Minutes() float64 }) string {
	if d.Minutes() < 1 {
		return map[bool]string{true: "ahora", false: "now"}[lang == "es"]
	}
	return agoIn(lang, d)
}

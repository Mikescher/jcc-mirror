package app

import (
	_ "embed"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/store"
)

//go:embed setup.html
var setupHTML string

// setupTmpl is the whole dashboard until M6 replaces it with the Angular build.
// It is plain forms and no script on purpose: the one thing it has to do is work
// on a first boot, from a phone, over a tunnel that may itself be the problem.
var setupTmpl = template.Must(template.New("setup").Funcs(template.FuncMap{
	"bytes":    format.Bytes,
	"duration": func(seconds float64) string { return format.Duration(time.Duration(seconds * float64(time.Second))) },
	"uptime":   func(seconds int64) string { return format.Duration(time.Duration(seconds) * time.Second) },
	"clock":    func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") },
}).Parse(setupHTML))

type pageData struct {
	Status     Status
	Groups     []configGroup
	Events     []store.Event
	Audit      []store.AuditEntry
	Authed     bool
	Error      string
	TunnelPort int
}

type configGroup struct {
	Name   string
	Fields []configField
}

type configField struct {
	store.KeyDef
	Value string
	IsSet bool
}

func (a *App) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	a.renderSetupPage(w, r, http.StatusOK, "")
}

func (a *App) renderSetupPage(w http.ResponseWriter, r *http.Request, code int, errMsg string) {
	ctx := r.Context()

	data := pageData{
		Status:     a.Status(ctx),
		Authed:     a.authenticated(r),
		Error:      errMsg,
		TunnelPort: a.opts.TunnelPort,
	}

	values, err := a.store.Config(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.Groups = groupFields(values)

	if data.Events, err = a.store.Events(ctx, store.EventFilter{Limit: 25}); err != nil {
		a.log.Errorf("dashboard: %v", err)
	}
	if data.Audit, err = a.store.ConfigAudit(ctx, 15); err != nil {
		a.log.Errorf("dashboard: %v", err)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := setupTmpl.Execute(w, data); err != nil {
		a.log.Errorf("dashboard: render: %v", err)
	}
}

// groupFields turns the key registry into the form. Generated settings are left
// out: the private key never leaves the volume, and the token is read from the
// container log, not from a page anyone can open.
func groupFields(values store.Values) []configGroup {
	byGroup := map[string]*configGroup{}
	var order []string

	for _, d := range store.Keys() {
		if d.Generated {
			continue
		}
		g, ok := byGroup[d.Group]
		if !ok {
			g = &configGroup{Name: d.Group}
			byGroup[d.Group] = g
			order = append(order, d.Group)
		}
		f := configField{KeyDef: d, IsSet: strings.TrimSpace(values.Get(d.Name)) != ""}
		if !d.Secret {
			f.Value = values.Get(d.Name)
		}
		g.Fields = append(g.Fields, f)
	}

	out := make([]configGroup, 0, len(order))
	for _, name := range order {
		out = append(out, *byGroup[name])
	}
	sort.SliceStable(out, func(i, j int) bool { return groupRank(out[i].Name) < groupRank(out[j].Name) })
	return out
}

// groupRank puts the setup in the order a first boot needs it: the tunnel has to
// work before the remote can be reached.
func groupRank(name string) int {
	switch name {
	case "Tunnel":
		return 0
	case "Remote":
		return 1
	default:
		return 2
	}
}

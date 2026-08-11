package httpserver

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/ricsam/opencode-workspaces/internal/model"
	webassets "github.com/ricsam/opencode-workspaces/internal/web"
)

func TestHomePasswordFormOnlyForLocalAccounts(t *testing.T) {
	tmpl, err := template.New("base").Funcs(template.FuncMap{"join": func(v []string) string { return strings.Join(v, ",") }}).ParseFS(
		webassets.Assets,
		"templates/base.html",
		"templates/home.html",
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name         string
		hasLocalAuth bool
		wantForm     bool
	}{
		{name: "OIDC-only account", hasLocalAuth: false, wantForm: false},
		{name: "local account", hasLocalAuth: true, wantForm: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			data := viewData{
				Brand:        model.Branding{Name: "OpenCode Workspaces"},
				User:         model.User{ID: "user-id", Username: "test-user"},
				HasLocalAuth: test.hasLocalAuth,
			}
			if err := tmpl.ExecuteTemplate(&output, "base", data); err != nil {
				t.Fatal(err)
			}
			hasForm := strings.Contains(output.String(), "Change local password")
			if hasForm != test.wantForm {
				t.Fatalf("password form presence = %t, want %t", hasForm, test.wantForm)
			}
		})
	}
}

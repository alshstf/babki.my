package category_test

import (
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// The owner lists the default set, adds, moves, renames and removes a
// category; a viewer reads them and changes nothing.
func TestTheCategoriesOverHTTP(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	category.NewHandler(category.NewStore(pool), auth, sm).Mount(srv)
	base, owner := apitest.Serve(t, srv.Handler())

	var list []apitypes.Category
	apitest.Decode(t, apitest.Do(t, owner, http.MethodGet, base+"/api/v1/categories", ""), &list)
	var transport apitypes.Category
	for _, c := range list {
		if c.Name == "Транспорт" {
			transport = c
		}
	}
	if len(list) < 20 || transport.Name == "" || !transport.ParentId.IsNull() {
		t.Fatalf("got %d categories, Транспорт %+v; want the default set with Транспорт on top", len(list), transport)
	}

	resp := apitest.Do(t, owner, http.MethodPost, base+"/api/v1/categories",
		`{"kind":"expense","name":"Самокаты","parent_id":"`+transport.Id.String()+`"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	var kick apitypes.Category
	apitest.Decode(t, resp, &kick)
	if kick.ParentId.MustGet() != transport.Id || kick.Kind != apitypes.Expense {
		t.Errorf("created %+v, want an expense under Транспорт", kick)
	}

	resp = apitest.Do(t, owner, http.MethodPatch, base+"/api/v1/categories/"+kick.Id.String(), `{"parent_id":null,"name":"Самокаты и велосипеды"}`)
	var moved apitypes.Category
	apitest.Decode(t, resp, &moved)
	if !moved.ParentId.IsNull() || moved.Name != "Самокаты и велосипеды" {
		t.Errorf("patched %+v, want it on the top level, renamed", moved)
	}

	for name, c := range map[string]struct {
		method, path, body string
		want               int
	}{
		"a taken name":         {http.MethodPost, "/api/v1/categories", `{"kind":"expense","name":"продукты"}`, http.StatusBadRequest},
		"a parent with others": {http.MethodDelete, "/api/v1/categories/" + transport.Id.String(), "", http.StatusBadRequest},
		"an unknown one":       {http.MethodDelete, "/api/v1/categories/00000000-0000-0000-0000-000000000001", "", http.StatusNotFound},
		"a malformed id":       {http.MethodPatch, "/api/v1/categories/x", `{}`, http.StatusBadRequest},
		"a free one":           {http.MethodDelete, "/api/v1/categories/" + kick.Id.String(), "", http.StatusNoContent},
	} {
		resp := apitest.Do(t, owner, c.method, base+c.path, c.body)
		_ = resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", name, resp.StatusCode, c.want)
		}
	}

	// A viewer reads and changes nothing.
	resp = apitest.Do(t, owner, http.MethodPost, base+"/api/v1/members",
		`{"username":"reader","display_name":"R","password":"secret123","role":"viewer"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add a viewer: %d", resp.StatusCode)
	}
	jar, _ := cookiejar.New(nil)
	viewer := &http.Client{Jar: jar}
	login, err := viewer.Post(base+"/api/v1/auth/login", "application/json", strings.NewReader(`{"username":"reader","password":"secret123"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = login.Body.Close()
	if r := apitest.Do(t, viewer, http.MethodGet, base+"/api/v1/categories", ""); r.StatusCode != http.StatusOK {
		t.Errorf("viewer list: %d", r.StatusCode)
	}
	if r := apitest.Do(t, viewer, http.MethodPost, base+"/api/v1/categories", `{"kind":"income","name":"x"}`); r.StatusCode != http.StatusForbidden {
		t.Errorf("viewer create: %d, want 403", r.StatusCode)
	}
}

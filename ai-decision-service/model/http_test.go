package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPostJSONSendsAPIKeyOnlyWhenSet(t *testing.T) {
	cases := []struct {
		name, apiKey, wantAuth string
	}{
		{name: "no key", apiKey: "", wantAuth: ""},
		{name: "key set", apiKey: "k-123", wantAuth: "Bearer k-123"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != c.wantAuth {
					t.Errorf("got Authorization %q, want %q", got, c.wantAuth)
				}
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer srv.Close()

			var reply struct {
				OK bool `json:"ok"`
			}
			err := postJSON(context.Background(), srv.Client(), srv.URL, c.apiKey, map[string]string{}, &reply)
			if err != nil || !reply.OK {
				t.Errorf("got error %v and reply %+v, want no error and ok", err, reply)
			}
		})
	}
}

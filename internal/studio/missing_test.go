package studio

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMissingNamesModelsTheGatewayLacks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"data":[{"id":"gpt-5.6-luna"},{"id":"` + defaultVideoModel + `"}]}`))
	}))
	defer srv.Close()
	got, err := New(srv.URL+"/v1", "k", t.TempDir()).Missing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "image("+defaultImageModel+")") || strings.Contains(joined, "video(") {
		t.Fatalf("missing = %v", got)
	}
}

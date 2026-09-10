package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

type fakeDB struct{ err error }

func (d fakeDB) PingContext(context.Context) error { return d.err }
func TestHealth(t *testing.T) {
	for _, tc := range []struct {
		name, path        string
		dbErr, storageErr error
		want              int
	}{
		{"live despite failed dependencies", "/health/live", errors.New("db"), errors.New("disk"), 200},
		{"ready", "/health/ready", nil, nil, 200},
		{"database unavailable", "/health/ready", errors.New("secret connection string"), nil, 503},
		{"archive unavailable", "/health/ready", nil, errors.New("disk full"), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			Handler(fakeDB{tc.dbErr}, func() error { return tc.storageErr }).ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != tc.want {
				t.Fatalf("got %d want %d", w.Code, tc.want)
			}
			if tc.want == 503 && w.Body.String() != "{\"status\":\"not_ready\"}\n" {
				t.Fatal("health response leaks detail")
			}
		})
	}
}

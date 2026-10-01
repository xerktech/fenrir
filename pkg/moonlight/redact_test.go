package moonlight

import (
	"bytes"
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"k8s.io/klog/v2"
)

// captureKlog sends klog output to a buffer for the rest of the test.
func captureKlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	for k, v := range map[string]string{"logtostderr": "false", "alsologtostderr": "false", "stderrthreshold": "FATAL"} {
		if err := fs.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	klog.SetOutput(&buf)
	t.Cleanup(func() {
		klog.Flush()
		_ = fs.Set("logtostderr", "true")
	})
	return &buf
}

// Every secret query parameter is redacted in what loggingMiddleware logs,
// while the handler still sees the real value.
func TestLoggingMiddlewareRedactsSecrets(t *testing.T) {
	buf := captureKlog(t)
	q := url.Values{"appid": {"firefox"}}
	for i, k := range secretQueryParams {
		q.Set(k, "SECRETVALUE"+string(rune('A'+i)))
	}

	var seen url.Values
	h := loggingMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.URL.Query() }))
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/launch?"+q.Encode(), http.NoBody)
	h.ServeHTTP(httptest.NewRecorder(), r)
	klog.Flush()

	logged := buf.String()
	if !strings.Contains(logged, "appid:[firefox]") {
		t.Fatalf("request not logged with its query: %q", logged)
	}
	for _, k := range secretQueryParams {
		if strings.Contains(logged, q.Get(k)) {
			t.Errorf("log contains %s's value: %q", k, logged)
		}
		if seen.Get(k) != q.Get(k) {
			t.Errorf("handler saw %s=%q, want the real value", k, seen.Get(k))
		}
	}
}

// Response bodies (the server's pairing secrets) are not logged.
func TestSendXMLDoesNotLogBody(t *testing.T) {
	buf := captureKlog(t)
	sendXML(httptest.NewRecorder(), PairingResponse{Response: Response{StatusCode: 200}, PairingSecret: "SERVERSECRET"})
	klog.Flush()
	if strings.Contains(buf.String(), "SERVERSECRET") {
		t.Errorf("response body logged: %q", buf.String())
	}
}

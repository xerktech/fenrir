package controllers

import (
	"bytes"
	"flag"
	"strings"
	"testing"

	"github.com/r3labs/sse/v2"
	"k8s.io/klog/v2"
)

// Wolf's session events carry the stream's AES key and IV; they must never
// reach the logs.
func TestLogEventOmitsData(t *testing.T) {
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	for k, v := range map[string]string{"logtostderr": "false", "alsologtostderr": "false", "stderrthreshold": "FATAL"} {
		if err := fs.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	klog.SetOutput(&buf)
	t.Cleanup(func() { _ = fs.Set("logtostderr", "true") })

	logEvent(&sse.Event{Event: []byte("StreamSession"), Data: []byte(`{"aes_key":"SECRETKEY0123","aes_iv":"SECRETIV42"}`)})
	klog.Flush()

	if out := buf.String(); strings.Contains(out, "SECRETKEY0123") || strings.Contains(out, "SECRETIV42") {
		t.Errorf("event data logged: %q", out)
	} else if !strings.Contains(out, "StreamSession") {
		t.Errorf("event not logged at all: %q", out)
	}
}

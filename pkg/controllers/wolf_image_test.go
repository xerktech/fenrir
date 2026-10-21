package controllers

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The chart's pinned Wolf image starts under wolfCommand: its Wolf binary is
// at wolfBinary and knows WOLF_HTTP_SERVERS, and /entrypoint.sh is there. The
// unit tests substitute wolfBinary themselves, so only this catches a Wolf
// image whose layout moved, which would end every Session at start.
// It pulls the image, so it runs only with WOLF_IMAGE_CHECK=1 (CI wolf-image.yml).
func TestPinnedWolfImageStartsUnderWolfCommand(t *testing.T) {
	if os.Getenv("WOLF_IMAGE_CHECK") != "1" {
		t.Skip("set WOLF_IMAGE_CHECK=1 to pull and check the chart's Wolf image")
	}
	values, err := os.ReadFile("../../charts/direwolf-operator/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*WOLF_IMAGE:\s*(\S+)\s*$`).FindSubmatch(values)
	if m == nil {
		t.Fatal("no WOLF_IMAGE in the chart's values.yaml")
	}
	script := strings.Replace(wolfCommand[2], "exec /entrypoint.sh", "test -x /entrypoint.sh && echo started", 1)
	if script == wolfCommand[2] {
		t.Fatal("wolfCommand no longer ends in exec /entrypoint.sh; update this test")
	}
	out, err := exec.CommandContext(t.Context(), "docker", "run", "--rm", "--entrypoint", wolfCommand[0],
		string(m[1]), wolfCommand[1], script).CombinedOutput()
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), "started") {
		t.Fatalf("%s under wolfCommand: %v\n%s", m[1], err, out)
	}
}

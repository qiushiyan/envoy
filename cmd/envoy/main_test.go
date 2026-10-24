package main

import (
	"os"
	"testing"

	"github.com/qiushiyan/envoy/internal/detach"
)

// TestMain makes this test binary envoy itself when it is started with one of
// the hidden argv entries: a dispatch run in process re-executes
// os.Executable(), which here is the test binary, so the real detach chain —
// waiter, intermediate, detached process, control descriptor — runs under the
// race detector exactly as it does in production.
//
// The environment is cleared first, as the binary-driven suite in tests/ does
// for its child: a developer's launcher (ENVOY_CODEX_CMD), session identity
// (CLAUDE_CODE_SESSION_ID) or Claude config directory would otherwise reach
// the engine and change what it prints.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == detach.SpawnEntry || os.Args[1] == detach.DetachedEntry) {
		os.Exit(run(os.Args[1:]))
	}
	kept := map[string]string{}
	for _, k := range []string{"PATH", "TMPDIR", "ENVOY_CONTRACT_OLD", "ENVOY_CONTRACT_UPDATE", "CLIKIT_UPDATE"} {
		if v, ok := os.LookupEnv(k); ok {
			kept[k] = v
		}
	}
	os.Clearenv()
	for k, v := range kept {
		os.Setenv(k, v)
	}
	// The race runtime otherwise sleeps a second before each helper process
	// exits; detection during the run is unaffected.
	os.Setenv("GORACE", "atexit_sleep_ms=0")
	basePATH = kept["PATH"]
	var err error
	if pkgDir, err = os.Getwd(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

package process

import (
	"bytes"
	"testing"
)

func TestKeeperSpawnCodec(t *testing.T) {
	payload, err := encodeSpawn("/tmp", []string{"sleep", "1"}, []string{"A=b"})
	if err != nil {
		t.Fatal(err)
	}
	dir, argv, env, err := decodeSpawn(payload)
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/tmp" || len(argv) != 2 || argv[1] != "1" || len(env) != 1 || env[0] != "A=b" {
		t.Fatalf("dir=%q argv=%v env=%v", dir, argv, env)
	}
	var buf bytes.Buffer
	if err := writeKeeperFrame(&buf, keeperFrameReady, nil); err != nil {
		t.Fatal(err)
	}
	kind, body, err := readKeeperFrame(&buf)
	if err != nil || kind != keeperFrameReady || len(body) != 0 {
		t.Fatalf("kind=%d body=%q err=%v", kind, body, err)
	}
}

package irohnet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A connection that has carried several times the 100 packets after which
// go-iroh's QUIC would update its keys for the first time has updated no
// key: the interval kosh sets is the one the QUIC code reads. What the
// connection did is read from the qlog traces go-iroh writes into QLOGDIR,
// the server's and the client's.
func TestNoKeyUpdateEarlyInAConnection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QLOGDIR", dir)

	l, _ := testListener(t, defaultTimeouts)
	c := dial(t, l)
	s := accept(t, l, c, "hello")
	// Each round trip is a packet or more in each direction.
	const trips = 400
	for range trips {
		if _, err := c.Write([]byte("ping")); err != nil {
			t.Fatalf("client write: %v", err)
		}
		readExactly(t, s, "ping")
		if _, err := s.Write([]byte("pong")); err != nil {
			t.Fatalf("server write: %v", err)
		}
		readExactly(t, c, "pong")
	}
	c.Close()
	s.Close()
	l.Close()

	// A trace is complete once it says how the connection closed.
	var traces map[string]string
	deadline := time.Now().Add(wait)
	for {
		traces = map[string]string{}
		files, err := filepath.Glob(filepath.Join(dir, "*.sqlog"))
		if err != nil {
			t.Fatal(err)
		}
		complete := 0
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			traces[filepath.Base(f)] = string(b)
			if strings.Contains(string(b), `"transport:connection_closed"`) {
				complete++
			}
		}
		if len(files) == 2 && complete == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d qlog traces in QLOGDIR, %d of them complete; want the server's and the client's", len(files), complete)
		}
		time.Sleep(5 * time.Millisecond)
	}

	for name, trace := range traces {
		sent := strings.Count(trace, `"name":"transport:packet_sent"`)
		rcvd := strings.Count(trace, `"name":"transport:packet_received"`)
		if sent < 3*100 && rcvd < 3*100 {
			t.Errorf("%s: %d packets sent and %d received: too few to tell", name, sent, rcvd)
		}
		for _, trigger := range []string{"local_update", "remote_update"} {
			if n := strings.Count(trace, `"trigger":"`+trigger+`"`); n != 0 {
				t.Errorf("%s: %d key_updated events with the trigger %s after %d packets sent and %d received, want none",
					name, n, trigger, sent, rcvd)
			}
		}
	}
}

// The variable kosh set holds kosh's value, and it is go-iroh's: before
// kosh set it, it held go-iroh's 100, or the package would not have
// started (deferFirstKeyUpdate, which refuses a variable that holds
// anything else, as it does now that it holds kosh's value).
func TestFirstKeyUpdateIntervalIsSet(t *testing.T) {
	if qngFirstKeyUpdateInterval != firstKeyUpdateInterval {
		t.Fatalf("go-iroh's FirstKeyUpdateInterval is %d, want %d", qngFirstKeyUpdateInterval, firstKeyUpdateInterval)
	}
	err := deferFirstKeyUpdate()
	if err == nil {
		t.Fatal("deferFirstKeyUpdate accepted a variable that does not hold go-iroh's default")
	}
	if !strings.Contains(err.Error(), "FirstKeyUpdateInterval is 100000, not 100") {
		t.Errorf("deferFirstKeyUpdate: %v", err)
	}
	if qngFirstKeyUpdateInterval != firstKeyUpdateInterval {
		t.Errorf("deferFirstKeyUpdate failed and left %d in the variable, want it untouched", qngFirstKeyUpdateInterval)
	}
	// No key update is put off beyond the regular one, which is far
	// below what an AEAD allows under one key (RFC 9001 section 6.6).
	if firstKeyUpdateInterval > 1<<23/8 {
		t.Errorf("firstKeyUpdateInterval is %d, more than an eighth of AES-GCM's 2^23 packets", firstKeyUpdateInterval)
	}
}

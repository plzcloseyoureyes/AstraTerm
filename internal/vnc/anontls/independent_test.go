package anontls

import (
	"fmt"
	"testing"

	"github.com/termstead/termstead/internal/vnc/anontls/anontlstest"
)

// TestAgainstIndependentServer runs every suite against anontlstest, a server written independently of this
// package (standard library primitives only), with and without encrypt-then-MAC and extended master secret.
func TestAgainstIndependentServer(t *testing.T) {
	for _, s := range suites {
		for _, mode := range []struct{ noETM, noEMS bool }{{false, false}, {true, false}, {false, true}} {
			if s.aead && mode.noETM {
				continue
			}
			t.Run(fmt.Sprintf("%s/etm=%v/ems=%v", s.name, !mode.noETM, !mode.noEMS), func(t *testing.T) {
				cc, sc := tcpPair(t)
				type result struct {
					neg *anontlstest.Negotiated
					err error
				}
				done := make(chan result, 1)
				go func() {
					tc, neg, err := anontlstest.Serve(sc, &anontlstest.Config{Suites: []uint16{s.id}, NoETM: mode.noETM, NoEMS: mode.noEMS})
					if err == nil {
						buf := make([]byte, 64<<10)
						for {
							n, rerr := tc.Read(buf)
							if n > 0 {
								if _, werr := tc.Write(buf[:n]); werr != nil {
									break
								}
							}
							if rerr != nil {
								break
							}
						}
					}
					done <- result{neg, err}
				}()
				c := Client(cc, nil)
				if err := c.Handshake(); err != nil {
					t.Fatal(err)
				}
				st := c.ConnectionState()
				if st.CipherSuite != s.id || st.EncryptThenMAC != (!mode.noETM && !s.aead) || st.ExtendedMasterSecret != !mode.noEMS {
					t.Fatalf("state %+v", st)
				}
				roundTrip(t, c)
				c.Close()
				if r := <-done; r.err != nil || !r.neg.ClientGroupsOK && s.kx == kxDH {
					t.Fatalf("server: %v %+v", r.err, r.neg)
				}
			})
		}
	}
}

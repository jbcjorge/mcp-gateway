package listener

import "testing"

func TestListen_BindsAddress(t *testing.T) {
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	if ln.Addr().String() == "" {
		t.Error("expected a bound address")
	}
}

func TestListen_InvalidAddress(t *testing.T) {
	if ln, err := Listen("not-a-valid-address"); err == nil {
		ln.Close()
		t.Fatal("expected an error for an invalid address")
	}
}

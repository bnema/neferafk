package auth

import (
	"bytes"
	"testing"

	"github.com/bnema/neferafk/internal/ports"
)

func TestBootstrapRoundTripAndRejects(t *testing.T) {
	in := ports.AuthBootstrap{Generation: 7, PINSource: ports.PINSourceEnv, PINReference: "ref", EnvPIN: []byte("123456")}
	var buf bytes.Buffer
	if err := WriteBootstrap(&buf, in); err != nil {
		t.Fatal(err)
	}
	valid := bytes.Clone(buf.Bytes())
	out, err := ReadBootstrap(&buf)
	if err != nil || out.Generation != 7 || out.PINSource != in.PINSource || out.PINReference != "ref" || string(out.EnvPIN) != "123456" {
		t.Fatalf("%+v %v", out, err)
	}
	if err := WriteBootstrap(&buf, ports.AuthBootstrap{}); err == nil {
		t.Fatal("zero generation encoded")
	}
	for _, mutate := range []func([]byte){
		func(b []byte) { b[0] = 'X' },
		func(b []byte) { b[7]++ },
		func(b []byte) { b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15] = 0, 0, 0, 0, 0, 0, 0, 0 },
	} {
		bad := bytes.Clone(valid)
		mutate(bad)
		if _, err := ReadBootstrap(bytes.NewReader(bad)); err == nil {
			t.Fatal("corrupt bootstrap accepted")
		}
	}
	if _, err := ReadBootstrap(bytes.NewReader(valid[:len(valid)-1])); err == nil {
		t.Fatal("truncated bootstrap accepted")
	}
}

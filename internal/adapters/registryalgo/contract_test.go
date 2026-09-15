package registryalgo

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The embedded programs and spec are copies of the AlgoKit build output;
// `make contract` refreshes them.
func TestEmbeddedProgramsMatchContractBuild(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "contract", "smart_contracts", "artifacts", "registry")
	for name, embedded := range map[string][]byte{
		"Registry.approval.teal": []byte(ApprovalTEAL),
		"Registry.approval.bin":  ApprovalProgram,
		"Registry.clear.teal":    []byte(ClearTEAL),
		"Registry.clear.bin":     ClearProgram,
		"Registry.arc56.json":    AppSpec,
	} {
		built, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(built, embedded) {
			t.Errorf("%s drifted from contract/ build output; run `make contract`", name)
		}
	}
}

func TestMethods(t *testing.T) {
	for _, tc := range []struct {
		sig, selector string
		got           string
		sel           []byte
	}{
		{"put(string,byte[],byte[],byte[],byte[])void", "4cc15367", PutMethod.GetSignature(), PutMethod.GetSelector()},
		{"remove(string)void", "8e8900b9", RemoveMethod.GetSignature(), RemoveMethod.GetSelector()},
	} {
		if tc.got != tc.sig {
			t.Errorf("method %s, want %s", tc.got, tc.sig)
		}
		if hex.EncodeToString(tc.sel) != tc.selector {
			t.Errorf("%s: selector %x, want %s", tc.sig, tc.sel, tc.selector)
		}
		// The router matches selectors, so they are in the program.
		if !bytes.Contains(ApprovalProgram, tc.sel) {
			t.Errorf("%s: selector not in approval program", tc.sig)
		}
	}
	if len(ApprovalProgram) == 0 || ApprovalProgram[0] < 8 {
		t.Fatalf("approval program must target AVM >= 8 (boxes), got version %d", ApprovalProgram[0])
	}
}

func TestBoxMinBalance(t *testing.T) {
	if BoxMinBalance(8, 300) != 2500+400*308 {
		t.Fatal("mbr formula")
	}
}

func TestLimits(t *testing.T) {
	if BoxRefs != 8 {
		t.Fatalf("BoxRefs = %d", BoxRefs)
	}
	if got := MaxValueLen(48); got != 16322 {
		t.Fatalf("MaxValueLen(48) = %d", got)
	}
	if PutParts*MaxPartLen < MaxValueLen(0) {
		t.Fatal("parts cannot carry the largest value")
	}
}

// lenPrefix is the uint16 length that starts the ARC-4 encoding of a string or byte[].
func lenPrefix(b []byte) []byte {
	return binary.BigEndian.AppendUint16(nil, uint16(len(b)))
}

func TestPutArgs(t *testing.T) {
	id := "k44"
	for _, n := range []int{0, 1, MaxPartLen, MaxPartLen + 1, 3*MaxPartLen + 5, MaxValueLen(len(id))} {
		value := make([]byte, n)
		for i := range value {
			value[i] = byte(i % 251)
		}
		args, err := PutArgs(id, value)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if len(args) != 2+PutParts {
			t.Fatalf("n=%d: %d args", n, len(args))
		}
		if !bytes.Equal(args[0], PutMethod.GetSelector()) {
			t.Fatalf("n=%d: selector %x", n, args[0])
		}
		if want := append(lenPrefix([]byte(id)), id...); !bytes.Equal(args[1], want) {
			t.Fatalf("n=%d: id arg %x", n, args[1])
		}
		var joined []byte
		total := 0
		for i, a := range args {
			total += len(a)
			if len(a) > MaxArgLen {
				t.Fatalf("n=%d: arg %d is %d bytes", n, i, len(a))
			}
			if i < 2 {
				continue
			}
			part := a[lengthPrefix:]
			if !bytes.Equal(a[:lengthPrefix], lenPrefix(part)) {
				t.Fatalf("n=%d: part %d length prefix %x for %d bytes", n, i-2, a[:2], len(part))
			}
			if i > 2 && len(part) > 0 && len(args[i-1]) != MaxArgLen {
				t.Fatalf("n=%d: part %d used before part %d was full", n, i-2, i-3)
			}
			joined = append(joined, part...)
		}
		if !bytes.Equal(joined, value) {
			t.Fatalf("n=%d: parts do not reassemble", n)
		}
		if total > MaxAppArgsLen {
			t.Fatalf("n=%d: args total %d", n, total)
		}
		if n == MaxValueLen(len(id)) && total != MaxAppArgsLen {
			t.Fatalf("largest value: args total %d, want %d", total, MaxAppArgsLen)
		}
	}
	if _, err := PutArgs(id, make([]byte, MaxValueLen(len(id))+1)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized value: err = %v", err)
	}
}

func TestRemoveArgs(t *testing.T) {
	args, err := RemoveArgs("k44")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]byte{RemoveMethod.GetSelector(), append(lenPrefix([]byte("k44")), "k44"...)}
	if len(args) != len(want) || !bytes.Equal(args[0], want[0]) || !bytes.Equal(args[1], want[1]) {
		t.Fatalf("args %x, want %x", args, want)
	}
}

// Values measured on a consensus v42 localnet.
func TestCallFee(t *testing.T) {
	id := "k44" // args = 4 + (2+3) + 4×2 + value
	overhead := MaxAppArgsLen - MaxValueLen(len(id))
	for argsLen, want := range map[int]uint64{overhead: 1000, 2048: 1000, 2049: 1001, 3048: 1100, 16384: 2434} {
		args, err := PutArgs(id, make([]byte, argsLen-overhead))
		if err != nil {
			t.Fatal(err)
		}
		if got := CallFee(1000, args); got != want {
			t.Errorf("args %d: fee %d, want %d", argsLen, got, want)
		}
	}
}

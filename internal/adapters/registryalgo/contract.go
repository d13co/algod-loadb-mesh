package registryalgo

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/algorand/go-algorand-sdk/v2/abi"
)

// The registry application, an ARC-4 contract. Boxes named n<id> hold one
// encrypted NodeRecord each. Only the creator may call its methods:
//
//	put(string id, byte[] part0, byte[] part1, byte[] part2, byte[] part3)void
//	remove(string id)void
//
// put stores the concatenated parts. Creation, update and deletion of the
// application are bare calls; update and deletion are creator only too.
//
// The source is the AlgoKit project in contract/ (Algorand TypeScript,
// compiled by puya-ts). `make contract` rebuilds it and copies the TEAL,
// bytecode and ARC-56 spec into program/; a test fails if the copies drift
// from the build. `registry init` prefers compiling ApprovalTEAL through the
// local algod when its developer API is enabled and only falls back to the
// embedded bytes otherwise.

//go:embed program/Registry.approval.teal
var ApprovalTEAL string

//go:embed program/Registry.approval.bin
var ApprovalProgram []byte

//go:embed program/Registry.clear.teal
var ClearTEAL string

//go:embed program/Registry.clear.bin
var ClearProgram []byte

// AppSpec is the ARC-56 description of the application.
//
//go:embed program/Registry.arc56.json
var AppSpec []byte

// The ABI methods, as declared by AppSpec.
var (
	PutMethod    = specMethod("put")
	RemoveMethod = specMethod("remove")
)

func specMethod(name string) abi.Method {
	var spec abi.Contract
	if err := json.Unmarshal(AppSpec, &spec); err != nil {
		panic(fmt.Sprintf("registryalgo: embedded ARC-56 spec: %v", err))
	}
	m, err := spec.GetMethodByName(name)
	if err != nil {
		panic(fmt.Sprintf("registryalgo: embedded ARC-56 spec: %v", err))
	}
	return m
}

// Protocol limits as of consensus v42 (AVM 13).
const (
	// MaxAppArgsLen is the limit on the summed length of all application
	// args of one call.
	MaxAppArgsLen = 16384
	// FreeAppArgsLen is how many arg bytes the minimum fee covers; each byte
	// beyond costs argByteSurcharge.
	FreeAppArgsLen = 2048
	// MaxArgLen is the limit on a single arg (the AVM's largest byte string),
	// which is why put takes its value in parts.
	MaxArgLen = 4096
	// argByteSurcharge is the extra fee per charged byte, in millionths of
	// the minimum fee.
	argByteSurcharge = 100
	// boxRefBudget is the box I/O (bytes of box value) one reference grants.
	boxRefBudget = 2048
	// BoxRefs is the number of references every put and remove carries. The
	// budget must cover both the value written and the one box_del reads,
	// and no value can exceed MaxAppArgsLen, so eight (the most a
	// transaction may carry) always suffice.
	BoxRefs = (MaxAppArgsLen + boxRefBudget - 1) / boxRefBudget
)

// ABI encoding of put's args.
const (
	selectorLen  = 4
	lengthPrefix = 2 // of string and byte[]
	// PutParts is the number of byte[] parts put takes.
	PutParts = 4
	// MaxPartLen is the largest part: one arg less its length prefix.
	MaxPartLen = MaxArgLen - lengthPrefix
)

// MaxValueLen is the largest value a put for an id of the given length can
// carry: its args are the selector, the id and four parts, the last two
// kinds with length prefixes. Four parts of MaxPartLen exceed it, so any
// value up to this length fits.
func MaxValueLen(idLen int) int {
	return MaxAppArgsLen - selectorLen - lengthPrefix - idLen - PutParts*lengthPrefix
}

// PutArgs builds the application args of put(id, value), filling each part
// before the next. value must be at most MaxValueLen(len(id)) bytes.
func PutArgs(id string, value []byte) ([][]byte, error) {
	if max := MaxValueLen(len(id)); len(value) > max {
		return nil, fmt.Errorf("value of %d bytes exceeds %d", len(value), max)
	}
	parts := make([]interface{}, 0, 1+PutParts)
	parts = append(parts, id)
	for i := 0; i < PutParts; i++ {
		n := min(len(value), MaxPartLen)
		parts = append(parts, value[:n])
		value = value[n:]
	}
	return encodeCall(PutMethod, parts)
}

// RemoveArgs builds the application args of remove(id).
func RemoveArgs(id string) ([][]byte, error) {
	return encodeCall(RemoveMethod, []interface{}{id})
}

func encodeCall(m abi.Method, values []interface{}) ([][]byte, error) {
	if len(values) != len(m.Args) {
		return nil, fmt.Errorf("%s takes %d args, got %d", m.Name, len(m.Args), len(values))
	}
	args := [][]byte{m.GetSelector()}
	for i, v := range values {
		typ, err := m.Args[i].GetTypeObject()
		if err != nil {
			return nil, err
		}
		enc, err := typ.Encode(v)
		if err != nil {
			return nil, fmt.Errorf("%s arg %s: %w", m.Name, m.Args[i].Name, err)
		}
		args = append(args, enc)
	}
	return args, nil
}

// CallFee is the fee an app call with the given args needs when the minimum
// fee is minFee: minFee × (1 + 0.0001 × bytes beyond FreeAppArgsLen), rounded
// up.
func CallFee(minFee uint64, args [][]byte) uint64 {
	n := 0
	for _, a := range args {
		n += len(a)
	}
	factor := uint64(1_000_000)
	if n > FreeAppArgsLen {
		factor += argByteSurcharge * uint64(n-FreeAppArgsLen)
	}
	return (minFee*factor + 999_999) / 1_000_000
}

// BoxMinBalance is the minimum balance the app account must hold per box:
// 2500 + 400 * (len(name) + len(value)) microalgos.
func BoxMinBalance(nameLen, valueLen int) uint64 {
	return 2500 + 400*uint64(nameLen+valueLen)
}

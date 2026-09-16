//go:build contract

package contract

import (
	"context"
	"crypto/ed25519"
	_ "embed"
	"strings"
	"testing"

	"github.com/algorand/go-algorand-sdk/v2/abi"
	"github.com/algorand/go-algorand-sdk/v2/client/v2/algod"
	"github.com/algorand/go-algorand-sdk/v2/client/v2/common/models"
	"github.com/algorand/go-algorand-sdk/v2/crypto"
	"github.com/algorand/go-algorand-sdk/v2/transaction"
	"github.com/algorand/go-algorand-sdk/v2/types"

	"github.com/d13co/algod-loadb-mesh/internal/adapters/registryalgo"
)

// The approval program of the previous registry build (commit d14b5e9),
// whose methods took the node id as a string and named boxes n<id>. Apps
// deployed from it are updated in place with `registry update`.
//
//go:embed testdata/registry-v0.approval.bin
var v0Approval []byte

// v0 sends calls in the previous build's interface, signed by seed's key.
type v0 struct {
	t      *testing.T
	c      context.Context
	ac     *algod.Client
	sk     ed25519.PrivateKey
	sender types.Address
}

func newV0(t *testing.T, c context.Context, seed []byte) *v0 {
	ac, err := algod.MakeClient(algodURL, localToken)
	if err != nil {
		t.Fatal(err)
	}
	sk := ed25519.NewKeyFromSeed(seed[:32])
	acct, _ := crypto.AccountFromPrivateKey(sk)
	return &v0{t: t, c: c, ac: ac, sk: sk, sender: acct.Address}
}

func (v *v0) send(tx types.Transaction) models.PendingTransactionInfoResponse {
	v.t.Helper()
	txid, stx, err := crypto.SignTransaction(v.sk, tx)
	if err != nil {
		v.t.Fatal(err)
	}
	if _, err := v.ac.SendRawTransaction(stx).Do(v.c); err != nil {
		v.t.Fatal(err)
	}
	info, err := transaction.WaitForConfirmation(v.ac, txid, 8, v.c)
	if err != nil {
		v.t.Fatal(err)
	}
	return info
}

func (v *v0) create() uint64 {
	v.t.Helper()
	sp, _ := v.ac.SuggestedParams().Do(v.c)
	tx, err := transaction.MakeApplicationCreateTx(false, v0Approval, registryalgo.ClearProgram, types.StateSchema{}, types.StateSchema{},
		nil, nil, nil, nil, sp, v.sender, nil, types.Digest{}, [32]byte{}, types.Address{})
	if err != nil {
		v.t.Fatal(err)
	}
	app := v.send(tx).ApplicationIndex
	pay, _ := transaction.MakePaymentTxn(v.sender.String(), crypto.GetApplicationAddress(app).String(), 1_000_000, nil, "", sp)
	v.send(pay)
	return app
}

// call invokes a v0 method; the id is ABI string encoded and names box n<id>.
func (v *v0) call(app uint64, sig, id string, extra ...[]byte) {
	v.t.Helper()
	m, err := abi.MethodFromSignature(sig)
	if err != nil {
		v.t.Fatal(err)
	}
	enc, _ := abi.TypeOf("string")
	idArg, _ := enc.Encode(id)
	args := append([][]byte{m.GetSelector(), idArg}, extra...)
	sp, _ := v.ac.SuggestedParams().Do(v.c)
	refs := []types.AppBoxReference{{Name: []byte("n" + id)}}
	tx, err := transaction.MakeApplicationNoOpTxWithBoxes(app, args, nil, nil, nil, refs, sp, v.sender, nil, types.Digest{}, [32]byte{}, types.Address{})
	if err != nil {
		v.t.Fatal(err)
	}
	v.send(tx)
}

func TestRegistryUpdateAndDeleteApps(t *testing.T) {
	c := ctx(t)
	seed := fundedSeed(t, c)
	old := newV0(t, c, seed)
	reg := newRegistry(t, 0, seed)

	// An app of the previous build holding a record in the old box format.
	app := old.create()
	emptyPart := []byte{0, 0}
	old.call(app, "put(string,byte[],byte[],byte[],byte[])void", "k44", []byte{0, 3, 'a', 'b', 'c'}, emptyPart, emptyPart, emptyPart)
	st, err := reg.Inspect(c, app)
	if err != nil {
		t.Fatal(err)
	}
	if st.UpToDate || st.Boxes != 1 || st.Foreign != 1 {
		t.Fatalf("before update: %+v", st)
	}

	// The new program could never remove that box, so the update is refused.
	if err := reg.Update(c, app); err == nil || !strings.Contains(err.Error(), "another format") {
		t.Fatalf("update with old boxes: err = %v", err)
	}
	old.call(app, "remove(string)void", "k44")

	if err := reg.Update(c, app); err != nil {
		t.Fatalf("update: %v", err)
	}
	if st, err = reg.Inspect(c, app); err != nil || !st.UpToDate || st.Boxes != 0 {
		t.Fatalf("after update: %+v, %v", st, err)
	}

	// Same id, new interface.
	reg.AppID = app
	if err := reg.Put(c, record("k44", 64)); err != nil {
		t.Fatalf("put after update: %v", err)
	}
	if recs, _, err := reg.List(c); err != nil || len(recs) != 1 || recs[0].ID != "k44" {
		t.Fatalf("list after update: %v %+v", err, recs)
	}

	// Another key holder can neither update nor delete it.
	intruder := newRegistry(t, 0, fundedSeed(t, c))
	if err := intruder.Update(c, app); err == nil || !strings.Contains(err.Error(), "created by") {
		t.Fatalf("intruder update: err = %v", err)
	}

	// Deleting refuses while records remain, then succeeds.
	if err := reg.DeleteApp(c, app); err == nil || !strings.Contains(err.Error(), "still holds 1 boxes") {
		t.Fatalf("delete with records: err = %v", err)
	}
	if err := intruder.DeleteApp(c, app); err == nil {
		t.Fatal("intruder delete succeeded")
	}
	if err := reg.Delete(c, "k44"); err != nil {
		t.Fatal(err)
	}
	if err := reg.DeleteApp(c, app); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := reg.Inspect(c, app); err == nil {
		t.Fatal("app still exists")
	}

	// A spare app of the previous build with no records deletes directly.
	spare := old.create()
	if err := reg.DeleteApp(c, spare); err != nil {
		t.Fatalf("delete v0 app: %v", err)
	}
}

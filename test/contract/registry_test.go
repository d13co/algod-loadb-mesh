//go:build contract

// Contract tests for the registry client against algokit localnet
// (`algokit localnet start`). Run with `make contract-test`. The contract's
// own semantics are covered by contract/ (npm test); these exercise the Go
// client paths of docs/REGISTRY_CONTRACT.md §8.
package contract

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/algorand/go-algorand-sdk/v2/client/kmd"
	"github.com/algorand/go-algorand-sdk/v2/client/v2/algod"
	"github.com/algorand/go-algorand-sdk/v2/crypto"
	"github.com/algorand/go-algorand-sdk/v2/transaction"

	"github.com/d13co/algod-loadb-mesh/internal/adapters/algodhttp"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/registryalgo"
	"github.com/d13co/algod-loadb-mesh/internal/domain"
)

var (
	algodURL   = env("LOCALNET_ALGOD_URL", "http://localhost:4001")
	kmdURL     = env("LOCALNET_KMD_URL", "http://localhost:4002")
	localToken = strings.Repeat("a", 64)
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return c
}

// fundedSeed returns a new account seed holding algos from the localnet
// dispenser (the richest account in KMD's default wallet).
func fundedSeed(t *testing.T, c context.Context) []byte {
	t.Helper()
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	reg, err := registryalgo.New(0, seed, "", nil, algodURL, localToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	ac, err := algod.MakeClient(algodURL, localToken)
	if err != nil {
		t.Fatal(err)
	}
	kc, err := kmd.MakeClient(kmdURL, localToken)
	if err != nil {
		t.Fatal(err)
	}
	wallets, err := kc.ListWallets()
	if err != nil {
		t.Skipf("localnet kmd unavailable (%v); run `algokit localnet start`", err)
	}
	var walletID string
	for _, w := range wallets.Wallets {
		if w.Name == "unencrypted-default-wallet" {
			walletID = w.ID
		}
	}
	h, err := kc.InitWalletHandle(walletID, "")
	if err != nil {
		t.Fatal(err)
	}
	defer kc.ReleaseWalletHandle(h.WalletHandleToken)
	keys, err := kc.ListKeys(h.WalletHandleToken)
	if err != nil {
		t.Fatal(err)
	}
	var dispenser string
	var best uint64
	for _, addr := range keys.Addresses {
		info, err := ac.AccountInformation(addr).Do(c)
		if err == nil && info.Amount > best {
			dispenser, best = addr, info.Amount
		}
	}
	sp, err := ac.SuggestedParams().Do(c)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := transaction.MakePaymentTxn(dispenser, reg.Sender(), 10_000_000, nil, "", sp)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := kc.SignTransaction(h.WalletHandleToken, "", tx)
	if err != nil {
		t.Fatal(err)
	}
	txid, err := ac.SendRawTransaction(signed.SignedTransaction).Do(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.WaitForConfirmation(ac, txid, 8, c); err != nil {
		t.Fatal(err)
	}
	return seed
}

func newRegistry(t *testing.T, appID uint64, seed []byte) *registryalgo.Registry {
	t.Helper()
	return newRekeyedRegistry(t, appID, seed, "")
}

func newRekeyedRegistry(t *testing.T, appID uint64, seed []byte, syncAddress string) *registryalgo.Registry {
	t.Helper()
	r, err := registryalgo.New(appID, seed, syncAddress, algodhttp.New(algodURL, localToken, nil), algodURL, localToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func record(id string, tokenLen int) domain.NodeRecord {
	return domain.NodeRecord{
		ID:        id,
		Network:   "dockernet-v1",
		Endpoints: []string{"http://10.0.0.1:8080", "http://10.0.1.1:8080"},
		Token:     strings.Repeat("t", tokenLen),
		Agent:     domain.AgentInfo{Addr: "10.0.0.1:4100"},
		Version:   1,
	}
}

func TestRegistryLifecycle(t *testing.T) {
	c := ctx(t)
	seed := fundedSeed(t, c)
	reg := newRegistry(t, 0, seed)

	appID, err := reg.Create(c)
	if err != nil {
		t.Fatal(err)
	}

	// Localnet enables the developer API, so Create used algod's own
	// assembly of the TEAL; it must equal the puya bytecode.
	ac, _ := algod.MakeClient(algodURL, localToken)
	app, err := ac.GetApplicationByID(appID).Do(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(app.Params.ApprovalProgram, registryalgo.ApprovalProgram) {
		t.Errorf("on-chain approval program differs from embedded bytecode")
	}
	if !bytes.Equal(app.Params.ClearStateProgram, registryalgo.ClearProgram) {
		t.Errorf("on-chain clear program differs from embedded bytecode")
	}

	list := func() map[string]domain.NodeRecord {
		recs, round, err := reg.List(c)
		if err != nil {
			t.Fatal(err)
		}
		if round == 0 {
			t.Error("List returned round 0")
		}
		out := map[string]domain.NodeRecord{}
		for _, r := range recs {
			out[r.ID] = r
		}
		return out
	}

	small := record("k44", 64)
	if err := reg.Put(c, small); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got := list()["k44"]; !got.StaticEqual(small) || got.Version != 1 {
		t.Fatalf("list after put: %+v", got)
	}

	// Grow past one arg (4 KB, so the value spans several parts and the fee carries
	// the per-byte surcharge), then shrink back: box_del must be able to read
	// the larger old value, which needs more than one box reference.
	large := record("k44", 9000)
	large.Version = 2
	if err := reg.Put(c, large); err != nil {
		t.Fatalf("put large: %v", err)
	}
	if got := list()["k44"]; got.Token != large.Token {
		t.Fatal("large record not stored")
	}
	small.Version = 3
	if err := reg.Put(c, small); err != nil {
		t.Fatalf("shrink: %v", err)
	}
	if got := list()["k44"]; !got.StaticEqual(small) || got.Version != 3 {
		t.Fatalf("list after shrink: %+v", got)
	}

	// Records beyond the app-args limit are refused before anything is sent.
	if err := reg.Put(c, record("huge", 17000)); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized put: err = %v", err)
	}

	if err := reg.Put(c, record("k49", 64)); err != nil {
		t.Fatal(err)
	}
	if got := list(); len(got) != 2 {
		t.Fatalf("want 2 records, got %d", len(got))
	}

	if err := reg.Put(c, large); err != nil { // leave a large box to delete
		t.Fatal(err)
	}
	if err := reg.Delete(c, "k44"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := reg.Delete(c, "k44"); err != nil {
		t.Fatalf("delete is idempotent: %v", err)
	}
	if got := list(); len(got) != 1 || got["k49"].ID != "k49" {
		t.Fatalf("after delete: %v", got)
	}

	// Another key holder cannot write: the approval program checks the creator.
	intruder := newRegistry(t, appID, fundedSeed(t, c))
	if err := intruder.Put(c, record("k44", 64)); err == nil {
		t.Fatal("non-creator put succeeded")
	}
	if err := intruder.Delete(c, "k49"); err == nil {
		t.Fatal("non-creator delete succeeded")
	}
	if got := list(); len(got) != 1 {
		t.Fatalf("intruder changed the registry: %v", got)
	}

	appAddr := crypto.GetApplicationAddress(appID)
	info, err := ac.AccountInformation(appAddr.String()).Do(c)
	if err != nil {
		t.Fatal(err)
	}
	if info.Amount < info.MinBalance {
		t.Fatalf("app account below min balance: %d < %d", info.Amount, info.MinBalance)
	}
}

// A sync account rekeyed to another key keeps working as creator when the
// new key is configured with registry.sync_address.
func TestRegistryRekeyedSyncAccount(t *testing.T) {
	c := ctx(t)
	origSeed := fundedSeed(t, c)
	orig := newRegistry(t, 0, origSeed)
	appID, err := orig.Create(c)
	if err != nil {
		t.Fatal(err)
	}

	newSeed := make([]byte, 32)
	if _, err := rand.Read(newSeed); err != nil {
		t.Fatal(err)
	}
	rekeyed := newRekeyedRegistry(t, appID, newSeed, orig.Sender())
	if rekeyed.Sender() != orig.Sender() || rekeyed.Signer() == orig.Sender() {
		t.Fatalf("sender %s signer %s", rekeyed.Sender(), rekeyed.Signer())
	}
	if err := rekeyed.CheckAuth(c); err == nil {
		t.Fatal("CheckAuth passed before the rekey")
	}

	ac, _ := algod.MakeClient(algodURL, localToken)
	sp, err := ac.SuggestedParams().Do(c)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := transaction.MakePaymentTxn(orig.Sender(), orig.Sender(), 0, nil, "", sp)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rekey(rekeyed.Signer()); err != nil {
		t.Fatal(err)
	}
	txid, stx, err := crypto.SignTransaction(ed25519.NewKeyFromSeed(origSeed), tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.SendRawTransaction(stx).Do(c); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.WaitForConfirmation(ac, txid, 8, c); err != nil {
		t.Fatal(err)
	}

	if err := rekeyed.CheckAuth(c); err != nil {
		t.Fatalf("CheckAuth after rekey: %v", err)
	}
	if err := orig.CheckAuth(c); err == nil {
		t.Fatal("old key still passes CheckAuth")
	}
	rec := record("k44", 64)
	if err := rekeyed.Put(c, rec); err != nil {
		t.Fatalf("rekeyed put: %v", err)
	}
	recs, _, err := rekeyed.List(c)
	if err != nil || len(recs) != 1 || !recs[0].StaticEqual(rec) {
		t.Fatalf("rekeyed list: %v %+v", err, recs)
	}
	if err := rekeyed.Delete(c, "k44"); err != nil {
		t.Fatalf("rekeyed delete: %v", err)
	}
	// Without sync_address the new key signs as itself, which is not the creator.
	if err := newRegistry(t, appID, newSeed).Put(c, rec); err == nil {
		t.Fatal("put without sync_address succeeded")
	}
}

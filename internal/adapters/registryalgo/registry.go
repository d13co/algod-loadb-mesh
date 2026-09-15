// Package registryalgo implements ports.Registry on an Algorand application
// whose boxes hold encrypted NodeRecords. Reads go through the plain algod
// REST API (any synced node); writes are app-call transactions signed with
// the sync account (or by its auth key, when the account is rekeyed).
package registryalgo

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/algorand/go-algorand-sdk/v2/client/v2/algod"
	"github.com/algorand/go-algorand-sdk/v2/crypto"
	"github.com/algorand/go-algorand-sdk/v2/transaction"
	"github.com/algorand/go-algorand-sdk/v2/types"

	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// Registry is the on-chain registry.
type Registry struct {
	AppID  uint64
	Key    domain.SyncKey
	Reader ports.AlgodClient // box reads
	sdk    *algod.Client     // transactions
	sk     ed25519.PrivateKey
	sender types.Address // the sync account: txn sender and app creator
	signer types.Address // address of sk; differs from sender when rekeyed
	Log    ports.Logger
	// MaxRecords bounds a List to protect against a runaway app.
	MaxRecords int
}

// New builds a registry client. url/token point at the algod used for
// transactions (normally the local node); reader is used for box reads.
// syncAddress is the sync account when it has been rekeyed to syncSeed's
// key; empty means the key's own address.
func New(appID uint64, syncSeed []byte, syncAddress string, reader ports.AlgodClient, url, token string, log ports.Logger) (*Registry, error) {
	key, err := domain.DeriveSyncKey(syncSeed)
	if err != nil {
		return nil, err
	}
	sk := ed25519.NewKeyFromSeed(syncSeed[:32])
	acct, err := crypto.AccountFromPrivateKey(sk)
	if err != nil {
		return nil, err
	}
	sender := acct.Address
	if syncAddress != "" {
		if sender, err = types.DecodeAddress(syncAddress); err != nil {
			return nil, fmt.Errorf("registry: sync address: %w", err)
		}
	}
	sdk, err := algod.MakeClient(strings.TrimSuffix(url, "/"), token)
	if err != nil {
		return nil, err
	}
	return &Registry{AppID: appID, Key: key, Reader: reader, sdk: sdk, sk: sk, sender: sender, signer: acct.Address, Log: log, MaxRecords: 1000}, nil
}

// Sender is the sync account's address.
func (r *Registry) Sender() string { return r.sender.String() }

// Signer is the address of the key that signs; it equals Sender unless the
// sync account is rekeyed. SignTransaction sets the auth address itself.
func (r *Registry) Signer() string { return r.signer.String() }

// CheckAuth verifies on chain that the sync key may sign for the sync
// account, so a wrong sync_address fails with a clear error instead of a
// rejected transaction.
func (r *Registry) CheckAuth(ctx context.Context) error {
	info, err := r.sdk.AccountInformation(r.sender.String()).Do(ctx)
	if err != nil {
		return fmt.Errorf("registry: sync account %s: %w", r.sender, err)
	}
	auth := r.sender.String()
	if info.AuthAddr != "" {
		auth = info.AuthAddr
	}
	if auth != r.signer.String() {
		return fmt.Errorf("registry: sync account %s is authorized by %s, but the sync key is %s", r.sender, auth, r.signer)
	}
	return nil
}

// List implements ports.Registry.
func (r *Registry) List(ctx context.Context) ([]domain.NodeRecord, uint64, error) {
	if r.AppID == 0 {
		return nil, 0, errors.New("registry: app id not configured")
	}
	names, err := r.Reader.BoxNames(ctx, r.AppID)
	if err != nil {
		return nil, 0, err
	}
	st, err := r.Reader.Status(ctx)
	if err != nil {
		return nil, 0, err
	}
	var recs []domain.NodeRecord
	for i, name := range names {
		if i >= r.MaxRecords {
			break
		}
		if !domain.IsRecordBox(name) {
			continue
		}
		blob, err := r.Reader.Box(ctx, r.AppID, name)
		if errors.Is(err, ports.ErrNotFound) {
			continue // deleted between list and get
		}
		if err != nil {
			return nil, 0, err
		}
		rec, err := domain.OpenRecord(r.Key, name, blob)
		if err != nil {
			if r.Log != nil {
				r.Log.Warn("registry: skipping unreadable box", "box", fmt.Sprintf("%x", name), "err", err)
			}
			continue
		}
		recs = append(recs, rec)
	}
	return recs, st.LastRound, nil
}

// Put implements ports.Registry: seal the record and call the app.
func (r *Registry) Put(ctx context.Context, rec domain.NodeRecord) error {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	blob, err := domain.SealRecord(r.Key, rec, nonce)
	if err != nil {
		return err
	}
	if len(blob) > MaxValueLen {
		return fmt.Errorf("registry: sealed record for %q is %d bytes, limit is %d", rec.ID, len(blob), MaxValueLen)
	}
	args, err := PutArgs(domain.BoxTag(r.Key, rec.ID), blob)
	if err != nil {
		return fmt.Errorf("registry: put %q: %w", rec.ID, err)
	}
	name := domain.BoxName(r.Key, rec.ID)
	if err := r.ensureFunded(ctx, len(name), len(blob)); err != nil {
		return err
	}
	return r.call(ctx, args, name)
}

// Delete implements ports.Registry.
func (r *Registry) Delete(ctx context.Context, id string) error {
	args, err := RemoveArgs(domain.BoxTag(r.Key, id))
	if err != nil {
		return fmt.Errorf("registry: remove %q: %w", id, err)
	}
	return r.call(ctx, args, domain.BoxName(r.Key, id))
}

func (r *Registry) call(ctx context.Context, args [][]byte, box []byte) error {
	sp, err := r.sdk.SuggestedParams().Do(ctx)
	if err != nil {
		return fmt.Errorf("registry: suggested params: %w", err)
	}
	// Each reference grants box I/O, which must cover the old value box_del
	// reads as well as the new one, so size by the largest possible box
	// rather than by this value.
	refs := make([]types.AppBoxReference, BoxRefs)
	for i := range refs {
		refs[i] = types.AppBoxReference{AppID: 0, Name: box}
	}
	tx, err := transaction.MakeApplicationNoOpTxWithBoxes(r.AppID, args, nil, nil, nil, refs, sp, r.sender, nil, types.Digest{}, [32]byte{}, types.Address{})
	if err != nil {
		return fmt.Errorf("registry: build txn: %w", err)
	}
	// Arg bytes beyond the free allowance raise the required fee.
	if fee := CallFee(sp.MinFee, args); uint64(tx.Fee) < fee {
		tx.Fee = types.MicroAlgos(fee)
	}
	txid, stx, err := crypto.SignTransaction(r.sk, tx)
	if err != nil {
		return err
	}
	if _, err := r.sdk.SendRawTransaction(stx).Do(ctx); err != nil {
		return fmt.Errorf("registry: send: %w", err)
	}
	if _, err := transaction.WaitForConfirmation(r.sdk, txid, 8, ctx); err != nil {
		return fmt.Errorf("registry: confirm %s: %w", txid, err)
	}
	return nil
}

// ensureFunded tops the app account up so the box minimum balance is met.
func (r *Registry) ensureFunded(ctx context.Context, nameLen, valueLen int) error {
	appAddr := crypto.GetApplicationAddress(r.AppID)
	info, err := r.sdk.AccountInformation(appAddr.String()).Do(ctx)
	if err != nil {
		return fmt.Errorf("registry: app account: %w", err)
	}
	need := info.MinBalance + BoxMinBalance(nameLen, valueLen) + 100_000
	if info.Amount >= need {
		return nil
	}
	sp, err := r.sdk.SuggestedParams().Do(ctx)
	if err != nil {
		return err
	}
	tx, err := transaction.MakePaymentTxn(r.sender.String(), appAddr.String(), need-info.Amount, []byte("algod-loadb-mesh box mbr"), "", sp)
	if err != nil {
		return err
	}
	txid, stx, err := crypto.SignTransaction(r.sk, tx)
	if err != nil {
		return err
	}
	if _, err := r.sdk.SendRawTransaction(stx).Do(ctx); err != nil {
		return fmt.Errorf("registry: fund app: %w", err)
	}
	_, err = transaction.WaitForConfirmation(r.sdk, txid, 8, ctx)
	return err
}

// Create deploys a new registry application and funds its account. It
// returns the application id. compileViaAlgod uses /v2/teal/compile when the
// node allows it, so the bytecode is the node's own assembly of ApprovalTEAL.
func (r *Registry) Create(ctx context.Context) (uint64, error) {
	approval, clear := ApprovalProgram, ClearProgram
	if resp, err := r.sdk.TealCompile([]byte(ApprovalTEAL)).Do(ctx); err == nil {
		if b, err := base64.StdEncoding.DecodeString(resp.Result); err == nil && len(b) > 0 {
			approval = b
			if resp2, err := r.sdk.TealCompile([]byte(ClearTEAL)).Do(ctx); err == nil {
				if c, err := base64.StdEncoding.DecodeString(resp2.Result); err == nil {
					clear = c
				}
			}
			if r.Log != nil {
				r.Log.Info("registry: programs compiled by algod", "approval_bytes", len(approval), "matches_embedded", string(approval) == string(ApprovalProgram))
			}
		}
	} else if r.Log != nil {
		r.Log.Info("registry: algod cannot compile TEAL (developer API off); using embedded bytecode")
	}
	sp, err := r.sdk.SuggestedParams().Do(ctx)
	if err != nil {
		return 0, err
	}
	tx, err := transaction.MakeApplicationCreateTxWithBoxes(false, approval, clear, types.StateSchema{}, types.StateSchema{}, 0,
		nil, nil, nil, nil, nil, sp, r.sender, []byte("algod-loadb-mesh registry"), types.Digest{}, [32]byte{}, types.Address{})
	if err != nil {
		return 0, err
	}
	txid, stx, err := crypto.SignTransaction(r.sk, tx)
	if err != nil {
		return 0, err
	}
	if _, err := r.sdk.SendRawTransaction(stx).Do(ctx); err != nil {
		return 0, fmt.Errorf("registry: create app: %w", err)
	}
	info, err := transaction.WaitForConfirmation(r.sdk, txid, 8, ctx)
	if err != nil {
		return 0, err
	}
	r.AppID = info.ApplicationIndex
	if err := r.ensureFunded(ctx, 0, 0); err != nil {
		return r.AppID, err
	}
	return r.AppID, nil
}

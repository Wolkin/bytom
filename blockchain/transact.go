package blockchain

import (
	"context"
	"encoding/json"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/bytom/blockchain/txbuilder"
	"github.com/bytom/errors"
	"github.com/bytom/net/http/httperror"
	"github.com/bytom/net/http/reqid"
	"github.com/bytom/protocol/bc/legacy"
)

var defaultTxTTL = 5 * time.Minute

func (bcr *BlockchainReactor) actionDecoder(action string) (func([]byte) (txbuilder.Action, error), bool) {
	var decoder func([]byte) (txbuilder.Action, error)
	switch action {
	case "control_account":
		decoder = bcr.accounts.DecodeControlAction
	case "control_address":
		decoder = txbuilder.DecodeControlAddressAction
	case "control_program":
		decoder = txbuilder.DecodeControlProgramAction
	case "control_receiver":
		decoder = txbuilder.DecodeControlReceiverAction
	case "issue":
		decoder = bcr.assets.DecodeIssueAction
	case "retire":
		decoder = txbuilder.DecodeRetireAction
	case "spend_account":
		decoder = bcr.accounts.DecodeSpendAction
	case "spend_account_unspent_output":
		decoder = bcr.accounts.DecodeSpendUTXOAction
	case "set_transaction_reference_data":
		decoder = txbuilder.DecodeSetTxRefDataAction
	default:
		return nil, false
	}
	return decoder, true
}

func (bcr *BlockchainReactor) buildSingle(ctx context.Context, req *BuildRequest) (*txbuilder.Template, error) {
	err := bcr.filterAliases(ctx, req)
	if err != nil {
		return nil, err
	}
	actions := make([]txbuilder.Action, 0, len(req.Actions))
	for i, act := range req.Actions {
		typ, ok := act["type"].(string)
		if !ok {
			return nil, errors.WithDetailf(errBadActionType, "no action type provided on action %d", i)
		}
		decoder, ok := bcr.actionDecoder(typ)
		if !ok {
			return nil, errors.WithDetailf(errBadActionType, "unknown action type %q on action %d", typ, i)
		}

		// Remarshal to JSON, the action may have been modified when we
		// filtered aliases.
		b, err := json.Marshal(act)
		if err != nil {
			return nil, err
		}
		action, err := decoder(b)
		if err != nil {
			return nil, errors.WithDetailf(errBadAction, "%s on action %d", err.Error(), i)
		}
		actions = append(actions, action)
	}

	ttl := req.TTL.Duration
	if ttl == 0 {
		ttl = defaultTxTTL
	}
	maxTime := time.Now().Add(ttl)

	tpl, err := txbuilder.Build(ctx, req.Tx, actions, maxTime)
	if errors.Root(err) == txbuilder.ErrAction {
		// Format each of the inner errors contained in the data.
		var formattedErrs []httperror.Response
		for _, innerErr := range errors.Data(err)["actions"].([]error) {
			resp := errorFormatter.Format(innerErr)
			formattedErrs = append(formattedErrs, resp)
		}
		err = errors.WithData(err, "actions", formattedErrs)
	}
	if err != nil {
		return nil, err
	}

	// ensure null is never returned for signing instructions
	if tpl.SigningInstructions == nil {
		tpl.SigningInstructions = []*txbuilder.SigningInstruction{}
	}
	return tpl, nil
}

// POST /build-transaction
func (bcr *BlockchainReactor) build(ctx context.Context, buildReqs *BuildRequest) Response {

	subctx := reqid.NewSubContext(ctx, reqid.New())

	tmpl, err := bcr.buildSingle(subctx, buildReqs)
	if err != nil {
		return resWrapper(nil, err)
	}

	return resWrapper(tmpl)
}

func (bcr *BlockchainReactor) submitSingle(ctx context.Context, tpl *txbuilder.Template) (map[string]string, error) {
	if tpl.Transaction == nil {
		return nil, errors.Wrap(txbuilder.ErrMissingRawTx)
	}

	err := txbuilder.FinalizeTx(ctx, bcr.chain, tpl.Transaction)
	if err != nil {
		return nil, errors.Wrapf(err, "tx %s", tpl.Transaction.ID.String())
	}

	return map[string]string{"txid": tpl.Transaction.ID.String()}, nil
}

// finalizeTxWait calls FinalizeTx and then waits for confirmation of
// the transaction.  A nil error return means the transaction is
// confirmed on the blockchain.  ErrRejected means a conflicting tx is
// on the blockchain.  context.DeadlineExceeded means ctx is an
// expiring context that timed out.
func (bcr *BlockchainReactor) finalizeTxWait(ctx context.Context, txTemplate *txbuilder.Template, waitUntil string) error {
	// Use the current generator height as the lower bound of the block height
	// that the transaction may appear in.
	localHeight := bcr.chain.Height()
	//generatorHeight := localHeight

	log.WithField("localHeight", localHeight).Info("Starting to finalize transaction")

	err := txbuilder.FinalizeTx(ctx, bcr.chain, txTemplate.Transaction)
	if err != nil {
		return err
	}
	if waitUntil == "none" {
		return nil
	}

	//TODO:complete finalizeTxWait
	//height, err := a.waitForTxInBlock(ctx, txTemplate.Transaction, generatorHeight)
	if err != nil {
		return err
	}
	if waitUntil == "confirmed" {
		return nil
	}

	return nil
}

func (bcr *BlockchainReactor) waitForTxInBlock(ctx context.Context, tx *legacy.Tx, height uint64) (uint64, error) {
	log.Printf("waitForTxInBlock function")
	for {
		height++
		select {
		case <-ctx.Done():
			return 0, ctx.Err()

		case <-bcr.chain.BlockWaiter(height):
			b, err := bcr.chain.GetBlockByHeight(height)
			if err != nil {
				return 0, errors.Wrap(err, "getting block that just landed")
			}
			for _, confirmed := range b.Transactions {
				if confirmed.ID == tx.ID {
					// confirmed
					return height, nil
				}
			}

			// might still be in pool or might be rejected; we can't
			// tell definitively until its max time elapses.
			// Re-insert into the pool in case it was dropped.
			err = txbuilder.FinalizeTx(ctx, bcr.chain, tx)
			if err != nil {
				return 0, err
			}

			// TODO(jackson): Do simple rejection checks like checking if
			// the tx's blockchain prevouts still exist in the state tree.
		}
	}
}

// POST /submit-transaction
func (bcr *BlockchainReactor) submit(ctx context.Context, tpl *txbuilder.Template) Response {

	txid, err := bcr.submitSingle(nil, tpl)
	if err != nil {
		log.WithField("err", err).Error("submit single tx")
		return resWrapper(nil, err)
	}

	log.WithField("txid", txid).Info("submit single tx")
	return resWrapper(txid)
}

// POST /sign-submit-transaction
func (bcr *BlockchainReactor) signSubmit(ctx context.Context, x struct {
	Auth string             `json:"auth"`
	Txs  txbuilder.Template `json:"transaction"`
}) Response {

	var err error
	if err = txbuilder.Sign(ctx, &x.Txs, nil, x.Auth, bcr.pseudohsmSignTemplate); err != nil {
		log.WithField("build err", err).Error("fail on sign transaction.")
		return resWrapper(nil, err)
	}

	log.Info("Sign Transaction complete.")

	txID, err := bcr.submitSingle(nil, &x.Txs)
	if err != nil {
		log.WithField("err", err).Error("submit single tx")
		return resWrapper(nil, err)
	}

	log.WithField("txid", txID["txid"]).Info("submit single tx")
	return resWrapper(txID)
}

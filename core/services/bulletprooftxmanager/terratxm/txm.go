package terratxm

import (
	"fmt"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/smartcontractkit/terra.go/msg"

	"github.com/smartcontractkit/chainlink/core/services/keystore"
	wasmtypes "github.com/terra-money/core/x/wasm/types"

	terraclient "github.com/smartcontractkit/chainlink-terra/pkg/terra/client"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/services"
	"github.com/smartcontractkit/chainlink/core/services/pg"
	"github.com/smartcontractkit/chainlink/core/utils"
	"github.com/smartcontractkit/sqlx"
)

var _ services.Service = (*Txm)(nil)

type Txm struct {
	starter    utils.StartStopOnce
	eb         pg.EventBroadcaster
	sub        pg.Subscription
	ticker     *time.Ticker
	orm        *ORM
	lggr       logger.Logger
	tc         terraclient.ReaderWriter
	ks         keystore.Terra
	stop, done chan struct{}
}

func NewTxm(db *sqlx.DB, tc terraclient.ReaderWriter, ks keystore.Terra, lggr logger.Logger, cfg pg.LogConfig, eb pg.EventBroadcaster, pollPeriod time.Duration) *Txm {
	ticker := time.NewTicker(pollPeriod)
	return &Txm{
		starter: utils.StartStopOnce{},
		eb:      eb,
		orm:     NewORM(db, lggr, cfg),
		ks:      ks,
		ticker:  ticker,
		tc:      tc,
		lggr:    lggr,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (txm *Txm) Start() error {
	return txm.starter.StartOnce("terratxm", func() error {
		sub, err := txm.eb.Subscribe(pg.ChannelInsertOnTerraMsg, "")
		if err != nil {
			return err
		}
		txm.sub = sub
		go txm.run(sub)
		return nil
	})
}

func (txm *Txm) run(sub pg.Subscription) {
	defer func() { txm.done <- struct{}{} }()
	for {
		select {
		case <-sub.Events():
			txm.sendMsgBatch()
		case <-txm.ticker.C:
			txm.sendMsgBatch()
		case <-txm.stop:
			txm.sub.Close()
			return
		}
	}
}

func (txm *Txm) sendMsgBatch() {
	unstarted, err := txm.orm.SelectMsgsWithState(Unstarted)
	if err != nil {
		txm.lggr.Errorw("unable to read unstarted msgs", "err", err)
		return
	}
	if len(unstarted) == 0 {
		return
	}
	txm.lggr.Infow("building a batch", "batch", unstarted)
	var msgsByFrom = make(map[string][]msg.Msg)
	var idsByFrom = make(map[string][]int64)
	for _, m := range unstarted {
		var ms wasmtypes.MsgExecuteContract
		err := ms.Unmarshal(m.Msg)
		if err != nil {
			// TODO
		}
		// TODO: simulate and discard if fails
		msgsByFrom[ms.Sender] = append(msgsByFrom[ms.Sender], &ms)
		idsByFrom[ms.Sender] = append(idsByFrom[ms.Sender], m.ID)
	}

	txm.lggr.Debugw("msgsByFrom", "msgsByFrom", msgsByFrom)
	gp := txm.tc.GasPrice()
	for s, msgs := range msgsByFrom {
		sender, _ := sdk.AccAddressFromBech32(s)
		an, sn, err := txm.tc.Account(sender)
		if err != nil {
			txm.lggr.Errorw("to read account", "err", err, "from", sender.String())
			continue
		}
		key, err := txm.ks.Get(sender.String())
		if err != nil {
			txm.lggr.Errorw("unable to find key for from address", "err", err, "from", sender.String())
			continue
		}
		privKey := NewPrivKey(key)
		txm.lggr.Debugw("sending a tx", "from", sender, "msgs", msgs)
		resp, err := txm.tc.SignAndBroadcast(msgs, an, sn, gp, privKey, txtypes.BroadcastMode_BROADCAST_MODE_BLOCK)
		if err != nil {
			txm.lggr.Errorw("error sending tx", "err", err, "resp", resp)
			continue
		}
		time.Sleep(1 * time.Second)
		// Confirm that this tx is onchain, ensuring the sequence number has incremented
		// so we can build a new batch
		txes, err := txm.tc.TxsEvents([]string{fmt.Sprintf("tx.hash='%s'", resp.TxResponse.TxHash)})
		if err != nil {
			txm.lggr.Errorw("error looking for hash of tx", "err", err, "resp", txes)
			continue
		}
		if txes == nil {
			continue
		}
		if len(txes.Txs) != 1 {
			txm.lggr.Errorw("expected one tx to be found", "txes", txes, "num", len(txes.Txs))
			continue
		}
		// Otherwise its definitely onchain, proceed to next batch
		err = txm.orm.UpdateMsgsWithState(idsByFrom[s], Completed)
		if err != nil {
			continue
		}
		txm.lggr.Infow("successfully sent batch", "hash", txes.TxResponses[0].TxHash, "msgs", msgs)
	}
}

func (txm *Txm) Enqueue(contractID string, msg []byte) (int64, error) {
	return txm.orm.InsertMsg(contractID, msg)
}

func (txm *Txm) Close() error {
	txm.stop <- struct{}{}
	<-txm.done
	return nil
}

func (txm *Txm) Healthy() error {
	return nil
}

func (txm *Txm) Ready() error {
	return nil
}
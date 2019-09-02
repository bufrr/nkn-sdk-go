package nkn_sdk_go

import (
	"errors"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/nknorg/nkn/chain"
	"github.com/nknorg/nkn/common"
	"github.com/nknorg/nkn/pb"
	"github.com/nknorg/nkn/transaction"
)

const defaultDuration = 4320

type NanoPay struct {
	w        *WalletSDK
	receiver common.Uint160
	duration uint32

	id         uint64
	expiration uint32
	amount     common.Fixed64
}

type NanoPayClaimer struct {
	sync.Mutex
	w          *WalletSDK
	tx         *transaction.Transaction
	id         *uint64
	expiration uint32
	amount     common.Fixed64
	closed     bool

	lastClaimTime     time.Time
	prevClaimedAmount common.Fixed64
}

func NewNanoPay(w *WalletSDK, receiver common.Uint160, duration ...uint32) *NanoPay {
	np := &NanoPay{w: w, receiver: receiver}
	if len(duration) > 0 {
		np.duration = duration[0]
	} else {
		np.duration = defaultDuration
	}
	return np
}

func (np *NanoPay) IncrementAmount(delta string) (*transaction.Transaction, error) {
	height, err := np.w.getHeight()
	if err != nil {
		return nil, err
	}
	if np.expiration == 0 || np.expiration <= height+5 {
		np.id = randUint64()
		np.expiration = height + np.duration
		np.amount = 0
	}
	deltaValue, err := common.StringToFixed64(delta)
	if err != nil {
		return nil, err
	}
	np.amount += deltaValue
	tx, err := transaction.NewNanoPayTransaction(np.w.account.ProgramHash, np.receiver, np.id, np.amount, np.expiration, np.expiration)
	if err != nil {
		return nil, err
	}

	return tx, nil
}

func NewNanoPayClaimer(w *WalletSDK, claimInterval time.Duration, errChan chan error) *NanoPayClaimer {
	npc := &NanoPayClaimer{w: w}
	go func() {
		for {
			var height uint32
			if npc.tx != nil && (npc.lastClaimTime.Add(claimInterval).After(time.Now()) || npc.expiration <= height+2) {
				var err error
				height, err = npc.w.getHeight()
				if err != nil {
					errChan <- err
					time.Sleep(time.Second)
					continue
				}
			}
			npc.Lock()
			if npc.tx != nil && (npc.lastClaimTime.Add(claimInterval).After(time.Now()) || npc.expiration <= height+2) {
				if err := npc.flush(); err != nil {
					errChan <- npc.closeWithError(err)
					break
				}
			}
			npc.Unlock()
			time.Sleep(time.Second)
		}
	}()
	return npc
}

func (npc *NanoPayClaimer) close() error {
	if npc.closed {
		return nil
	}
	npc.closed = true
	return npc.flush()
}

func (npc *NanoPayClaimer) Close() error {
	npc.Lock()
	defer npc.Unlock()
	return npc.close()
}

func (npc *NanoPayClaimer) IsClosed() bool {
	npc.Lock()
	defer npc.Unlock()
	return npc.closed
}

func (npc *NanoPayClaimer) closeWithError(err error) error {
	if err2 := npc.close(); err2 != nil {
		return multierror.Append(err, err2)
	}
	return err
}

func (npc *NanoPayClaimer) flush() error {
	if npc.tx != nil {
		_, err, _ := npc.w.SendRawTransaction(npc.tx)
		npc.tx = nil
		npc.expiration = 0
		npc.lastClaimTime = time.Now()
		return err
	}
	return nil
}

func (npc *NanoPayClaimer) Flush() error {
	npc.Lock()
	defer npc.Unlock()
	return npc.flush()
}

func (npc *NanoPayClaimer) Amount() common.Fixed64 {
	npc.Lock()
	defer npc.Unlock()
	return npc.prevClaimedAmount + npc.amount
}

func (npc *NanoPayClaimer) Claim(tx *transaction.Transaction) (common.Fixed64, error) {
	height, err := npc.w.getHeight()
	if err != nil {
		return 0, err
	}
	payload, err := transaction.Unpack(tx.UnsignedTx.Payload)
	if err != nil {
		return 0, npc.closeWithError(err)
	}
	npPayload, ok := payload.(*pb.NanoPay)
	if !ok {
		return 0, npc.closeWithError(errors.New("not nano pay tx"))
	}
	recipient, err := common.Uint160ParseFromBytes(npPayload.Recipient)
	if err != nil {
		return 0, npc.closeWithError(err)
	}
	if recipient.CompareTo(npc.w.account.ProgramHash) != 0 {
		return 0, npc.closeWithError(errors.New("wrong nano pay recipient"))
	}
	if err := chain.VerifyTransaction(tx); err != nil {
		return 0, npc.closeWithError(err)
	}
	sender, err := common.Uint160ParseFromBytes(npPayload.Sender)
	if err != nil {
		return 0, npc.closeWithError(err)
	}
	senderAddress, err := sender.ToAddress()
	if err != nil {
		return 0, npc.closeWithError(err)
	}
	senderBalance, err := npc.w.BalanceByAddress(senderAddress)
	if err != nil {
		return 0, err
	}
	npc.Lock()
	defer npc.Unlock()
	if npc.closed {
		return 0, errors.New("attempt to use closed nano pay claimer")
	}
	if npc.amount >= common.Fixed64(npPayload.Amount) {
		return 0, npc.closeWithError(errors.New("nano pay balance decreased"))
	}
	if *npc.id == npPayload.Id {
		if senderBalance < npc.amount {
			return 0, npc.closeWithError(errors.New("insufficient sender balance"))
		}
	} else {
		if err := npc.flush(); err != nil {
			return 0, npc.closeWithError(err)
		}
		npc.id = nil
		npc.prevClaimedAmount += npc.amount
		npc.amount = 0
	}
	if npPayload.TxnExpiration+3 >= height {
		return 0, npc.closeWithError(errors.New("nano pay tx expired"))
	}
	if npPayload.NanoPayExpiration+3 >= height {
		return 0, npc.closeWithError(errors.New("nano pay expired"))
	}
	npc.tx = tx
	npc.id = &npPayload.Id
	npc.expiration = npPayload.TxnExpiration
	npc.amount = common.Fixed64(npPayload.Amount)
	return npc.prevClaimedAmount + npc.amount, nil
}
package voucher

import (
	"errors"

	datatransfer "github.com/filecoin-project/go-data-transfer"
	"github.com/ipfs/go-cid"
	"github.com/ipld/go-ipld-prime"
	"github.com/libp2p/go-libp2p-core/peer"
)

//go:generate go run -tags cbg ./tools

var _ datatransfer.RequestValidator = &Validator{}

// A Voucher is used to communicate a new DAG head
type Voucher struct {
	C *cid.Cid
}

// Type provides an identifier for the voucher to go-data-transfer
func (v *Voucher) Type() datatransfer.TypeIdentifier {
	return "LegsVoucher"
}

// A VoucherResult responds to a voucher
type VoucherResult struct {
	Code uint64
}

// Type provides an identifier for the voucher result to go-data-transfer
func (v *VoucherResult) Type() datatransfer.TypeIdentifier {
	return "LegsVoucherResult"
}

type Validator struct {
	//ctx context.Context
	//ValidationsReceived chan receivedValidation
}

func (vl *Validator) ValidatePush(
	isRestart bool,
	chid datatransfer.ChannelID,
	sender peer.ID,
	voucher datatransfer.Voucher,
	baseCid cid.Cid,
	selector ipld.Node) (datatransfer.VoucherResult, error) {

	// This is a pull-only DT voucher.
	return nil, errors.New("invalid")
}

func (vl *Validator) ValidatePull(
	isRestart bool,
	chid datatransfer.ChannelID,
	receiver peer.ID,
	voucher datatransfer.Voucher,
	baseCid cid.Cid,
	selector ipld.Node) (datatransfer.VoucherResult, error) {

	return &VoucherResult{Code: 0}, nil
}

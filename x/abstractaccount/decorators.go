package abstractaccount

import (
	"encoding/json"

	"cosmossdk.io/errors"
	"github.com/cosmos/gogoproto/proto"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	txsigning "github.com/cosmos/cosmos-sdk/types/tx/signing"
	authante "github.com/cosmos/cosmos-sdk/x/auth/ante"
	authkeeper "github.com/cosmos/cosmos-sdk/x/auth/keeper"
	authsigning "github.com/cosmos/cosmos-sdk/x/auth/signing"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"

	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
	wasmvmtypes "github.com/CosmWasm/wasmvm/types"

	keeper "github.com/larry0x/abstract-account/x/abstractaccount/keeper"
	types "github.com/larry0x/abstract-account/x/abstractaccount/types"
)

var (
	_ sdk.AnteDecorator = (*BeforeTxDecorator)(nil)
	_ sdk.PostDecorator = (*AfterTxDecorator)(nil)
)

// --------------------------------- BeforeTx ----------------------------------

type BeforeTxDecorator struct {
	aak             keeper.Keeper
	ak              authkeeper.AccountKeeper
	ck              wasmtypes.ContractOpsKeeper
	signModeHandler authsigning.SignModeHandler
}

func NewBeforeTxDecorator(
	aak keeper.Keeper, ak authkeeper.AccountKeeper,
	ck wasmtypes.ContractOpsKeeper, signModeHandler authsigning.SignModeHandler,
) BeforeTxDecorator {
	return BeforeTxDecorator{aak, ak, ck, signModeHandler}
}

func (d BeforeTxDecorator) AnteHandle(ctx sdk.Context, tx sdk.Tx, simulate bool, next sdk.AnteHandler) (newCtx sdk.Context, err error) {
	// First we need to determine whether the rules of account abstraction should
	// apply to this tx. There are two criteria:
	//
	// - The tx has exactly one signer and one signature
	// - This one signer is an AbstractAccount
	//
	// Both criteria must be satisfied for this be to be qualified as an AA tx.
	isAbstractAccountTx, signerAcc, sig, err := isAbstractAccountTx(ctx, tx, d.ak)
	if err != nil {
		return ctx, err
	}

	// If the tx is an AA tx, we save the signer address to the module store.
	// We will use it in the PostHandler.
	//
	// If it's not an AA tx, we simply delegate the ante task to the default
	// SigVerificationDecorator.
	if isAbstractAccountTx {
		d.aak.SetSignerAddress(ctx, signerAcc.GetAddress())
	} else {
		svd := authante.NewSigVerificationDecorator(d.ak, d.signModeHandler)
		return svd.AnteHandle(ctx, tx, simulate, next)
	}

	// Now that we've determined the tx is an AA tx, let us prepare the SudoMsg
	// that will be used to invoke the account contract. The msg includes:
	//
	// - The messages in the tx, converted to cosmwasm_std::StargateMsg format
	// - The sign bytes
	// - The signature
	//
	// Firstly let's prepare the messages.
	stargateMsgs, err := sdkMsgsToStargateMsgs(tx.GetMsgs())
	if err != nil {
		return ctx, err
	}

	// Then let us the prepare the sign bytes and signature. Logics here are
	// mostly copied over from the SigVerificationDecorator.
	signBytes, sigBytes, err := prepareCredentials(ctx, tx, signerAcc, sig.Data, d.signModeHandler)
	if err != nil {
		return ctx, err
	}

	// Assemble the SudoMsg and serialize it into a JSON string
	sudoMsgBytes, err := json.Marshal(&types.AccountSudoMsg{
		BeforeTx: &types.BeforeTx{
			Msgs:      stargateMsgs,
			SignBytes: signBytes,
			Signature: sigBytes,
		},
	})
	if err != nil {
		return ctx, err
	}

	// Call the contract
	_, err = d.ck.Sudo(ctx, signerAcc.GetAddress(), sudoMsgBytes)
	if err != nil {
		return ctx, err
	}

	return next(ctx, tx, simulate)
}

// ---------------------------------- AfterTx ----------------------------------

type AfterTxDecorator struct {
	aak keeper.Keeper
	ak  authkeeper.AccountKeeper
	ck  wasmtypes.ContractOpsKeeper
}

func NewAfterTxDecorator(aak keeper.Keeper, ak authkeeper.AccountKeeper, ck wasmtypes.ContractOpsKeeper) AfterTxDecorator {
	return AfterTxDecorator{aak, ak, ck}
}

func (d AfterTxDecorator) PostHandle(ctx sdk.Context, tx sdk.Tx, simulate, success bool, next sdk.PostHandler) (newCtx sdk.Context, err error) {
	// Load the signer address, which we determined during the AnteHandler.
	//
	// If found, we delete it from module store since it's not needed for handling
	// the next tx.
	//
	// If not found, it means this tx is not an AA tx, in which case we skip.
	signerAddr := d.aak.GetSignerAddress(ctx)
	if signerAddr != nil {
		d.aak.DeleteSignerAddress(ctx)
	} else {
		return next(ctx, tx, simulate, success)
	}

	// Prepare the SudoMsg
	sudoMsgBytes, err := json.Marshal(&types.AccountSudoMsg{
		AfterTx: &types.AfterTx{
			Success: success,
		},
	})
	if err != nil {
		return ctx, err
	}

	// Call the contract
	_, err = d.ck.Sudo(ctx, signerAddr, sudoMsgBytes)
	if err != nil {
		return ctx, err
	}

	return next(ctx, tx, simulate, success)
}

// ---------------------------------- Helpers ----------------------------------

func isAbstractAccountTx(ctx sdk.Context, tx sdk.Tx, ak authkeeper.AccountKeeper) (bool, *types.AbstractAccount, txsigning.SignatureV2, error) {
	sigTx, ok := tx.(authsigning.SigVerifiableTx)
	if !ok {
		return false, nil, txsigning.SignatureV2{}, errors.Wrap(sdkerrors.ErrTxDecode, "tx is not a SigVerifiableTx")
	}

	sigs, err := sigTx.GetSignaturesV2()
	if err != nil {
		return false, nil, txsigning.SignatureV2{}, err
	}

	signerAddrs := sigTx.GetSigners()
	if len(signerAddrs) != 1 || len(sigs) != 1 {
		return false, nil, txsigning.SignatureV2{}, nil
	}

	signerAcc, err := authante.GetSignerAcc(ctx, ak, signerAddrs[0])
	if err != nil {
		return false, nil, txsigning.SignatureV2{}, err
	}

	absAcc, ok := signerAcc.(*types.AbstractAccount)
	if !ok {
		return false, nil, txsigning.SignatureV2{}, nil
	}

	return true, absAcc, sigs[0], nil
}

func prepareCredentials(
	ctx sdk.Context, tx sdk.Tx, signerAcc authtypes.AccountI,
	sigData txsigning.SignatureData, handler authsigning.SignModeHandler,
) ([]byte, []byte, error) {
	signerData := authsigning.SignerData{
		Address:       signerAcc.GetAddress().String(),
		ChainID:       ctx.ChainID(),
		AccountNumber: signerAcc.GetAccountNumber(), // should we handle the case that this is a gentx?
		Sequence:      signerAcc.GetSequence(),
		PubKey:        signerAcc.GetPubKey(),
	}

	data, ok := sigData.(*txsigning.SingleSignatureData)
	if !ok {
		return nil, nil, types.ErrNotSingleSignautre
	}

	signBytes, err := handler.GetSignBytes(data.SignMode, signerData, tx)
	if err != nil {
		return nil, nil, err
	}

	return signBytes, data.Signature, nil
}

func sdkMsgsToStargateMsgs(msgs []sdk.Msg) ([]wasmvmtypes.StargateMsg, error) {
	stargateMsgs := []wasmvmtypes.StargateMsg{}

	for _, msg := range msgs {
		bz, err := proto.Marshal(msg)
		if err != nil {
			return nil, err
		}

		stargateMsg := wasmvmtypes.StargateMsg{
			TypeURL: sdk.MsgTypeURL(msg),
			Value:   bz,
		}

		stargateMsgs = append(stargateMsgs, stargateMsg)
	}

	return stargateMsgs, nil
}
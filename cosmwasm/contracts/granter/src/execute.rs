use abstract_account::StargateMsg;
use cosmwasm_std::{Addr, Binary, Deps, Response, Storage, DepsMut, Env, MessageInfo, BlockInfo};
use cw_utils::Expiration;
use sha2::{Digest, Sha256};

use crate::{
    error::{ContractError, ContractResult},
    state::{PUBKEY, GRANTS}, msg::Grant,
};

pub fn init(store: &mut dyn Storage, pubkey: &Binary) -> ContractResult<Response> {
    PUBKEY.save(store, pubkey)?;

    Ok(Response::new()
        .add_attribute("method", "init")
        .add_attribute("pubkey", pubkey.to_base64()))
}

pub fn before_tx(
    deps: Deps,
    block: &BlockInfo,
    msgs: &[StargateMsg],
    pubkey: Option<&Binary>,
    sign_bytes: &Binary,
    signature: &Binary,
) -> ContractResult<Response> {
    let sign_bytes_hash = sha256(sign_bytes);
    let self_pubkey = PUBKEY.load(deps.storage)?;
    let pubkey = pubkey.unwrap_or(&self_pubkey);

    if *pubkey != self_pubkey {
        assert_has_grant(deps.storage, block, msgs, pubkey)?;
    }

    if !deps.api.secp256k1_verify(&sign_bytes_hash, signature, pubkey)? {
        return Err(ContractError::InvalidSignature);
    }

    Ok(Response::new()
        .add_attribute("method", "before_tx"))
}

pub fn after_tx() -> ContractResult<Response> {
    Ok(Response::new()
        .add_attribute("method", "after_tx"))
}

pub fn grant(
    deps: DepsMut,
    env: Env,
    info: MessageInfo,
    type_url: String,
    grantee: Binary,
    expiry: Option<Expiration>,
) -> ContractResult<Response> {
    // only the account itself can make grants
    assert_self(&info.sender, &env.contract.address)?;

    // the grant can't be already expired
    if let Some(expiry) = expiry.as_ref() {
        if expiry.is_expired(&env.block) {
            return Err(ContractError::NewGrantExpired);
        }
    }

    GRANTS.save(deps.storage, (&type_url, &grantee), &Grant { expiry })?;

    Ok(Response::new()
        .add_attribute("method", "grant")
        .add_attribute("granter", env.contract.address)
        .add_attribute("grantee", grantee.to_base64())
        .add_attribute("type_url", type_url))
}

pub fn revoke(
    deps: DepsMut,
    env: Env,
    info: MessageInfo,
    type_url: String,
    grantee: Binary,
) -> ContractResult<Response> {
    // only the account itself can revoke grants
    assert_self(&info.sender, &env.contract.address)?;

    GRANTS.remove(deps.storage, (&type_url, &grantee));

    Ok(Response::new()
        .add_attribute("method", "revoke")
        .add_attribute("granter", env.contract.address)
        .add_attribute("grantee", grantee.to_base64())
        .add_attribute("type_url", type_url))
}

fn assert_has_grant(
    store: &dyn Storage,
    block: &BlockInfo,
    msgs: &[StargateMsg],
    grantee: &Binary,
) -> ContractResult<()> {
    for msg in msgs {
        let Some(grant) = GRANTS.may_load(store, (&msg.type_url, grantee))? else {
            return Err(ContractError::GrantNotFound {
                type_url: msg.type_url.clone(),
                grantee: grantee.to_base64(),
            });
        };

        if let Some(expiry) = grant.expiry {
            if expiry.is_expired(block) {
                return Err(ContractError::GrantExpired {
                    type_url: msg.type_url.clone(),
                    grantee: grantee.to_base64(),
                });
            }
        }
    }

    Ok(())
}

fn assert_self(sender: &Addr, contract: &Addr) -> ContractResult<()> {
    if sender != contract {
        return Err(ContractError::Unauthorized);
    }

    Ok(())
}

fn sha256(msg: &[u8]) -> Vec<u8> {
    let mut hasher = Sha256::new();
    hasher.update(msg);
    hasher.finalize().to_vec()
}
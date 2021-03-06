package main

import (
	"fmt"
	"io/ioutil"
	"time"

	"github.com/bitum-project/bitumd/chaincfg/chainhash"
	"github.com/bitum-project/bitumd/rpcclient"
	"github.com/bitum-project/bitumstakepool/backend/stakepoold/rpc/rpcserver"
	"github.com/bitum-project/bitumstakepool/backend/stakepoold/userdata"
)

var requiredChainServerAPI = semver{major: 5, minor: 1, patch: 0}
var requiredWalletAPI = semver{major: 6, minor: 0, patch: 0}

func connectNodeRPC(ctx *rpcserver.AppContext, cfg *config) (*rpcclient.Client, semver, error) {
	var nodeVer semver

	bitumdCert, err := ioutil.ReadFile(cfg.BitumdCert)
	if err != nil {
		log.Errorf("Failed to read bitumd cert file at %s: %s\n",
			cfg.BitumdCert, err.Error())
		return nil, nodeVer, err
	}

	log.Debugf("Attempting to connect to bitumd RPC %s as user %s "+
		"using certificate located in %s",
		cfg.BitumdHost, cfg.BitumdUser, cfg.BitumdCert)

	connCfgDaemon := &rpcclient.ConnConfig{
		Host:         cfg.BitumdHost,
		Endpoint:     "ws", // websocket
		User:         cfg.BitumdUser,
		Pass:         cfg.BitumdPassword,
		Certificates: bitumdCert,
	}

	ntfnHandlers := getNodeNtfnHandlers(ctx)
	bitumdClient, err := rpcclient.New(connCfgDaemon, ntfnHandlers)
	if err != nil {
		log.Errorf("Failed to start bitumd RPC client: %s\n", err.Error())
		return nil, nodeVer, err
	}

	// Ensure the RPC server has a compatible API version.
	ver, err := bitumdClient.Version()
	if err != nil {
		log.Error("Unable to get RPC version: ", err)
		return nil, nodeVer, fmt.Errorf("Unable to get node RPC version")
	}

	bitumdVer := ver["bitumdjsonrpcapi"]
	nodeVer = semver{bitumdVer.Major, bitumdVer.Minor, bitumdVer.Patch}

	if !semverCompatible(requiredChainServerAPI, nodeVer) {
		return nil, nodeVer, fmt.Errorf("Node JSON-RPC server does not have "+
			"a compatible API version. Advertises %v but require %v",
			nodeVer, requiredChainServerAPI)
	}

	return bitumdClient, nodeVer, nil
}

func connectWalletRPC(cfg *config) (*rpcclient.Client, semver, error) {
	var walletVer semver

	bitumwCert, err := ioutil.ReadFile(cfg.WalletCert)
	if err != nil {
		log.Errorf("Failed to read bitumwallet cert file at %s: %s\n",
			cfg.WalletCert, err.Error())
		return nil, walletVer, err
	}

	log.Infof("Attempting to connect to bitumwallet RPC %s as user %s "+
		"using certificate located in %s",
		cfg.WalletHost, cfg.WalletUser, cfg.WalletCert)

	connCfgWallet := &rpcclient.ConnConfig{
		Host:         cfg.WalletHost,
		Endpoint:     "ws",
		User:         cfg.WalletUser,
		Pass:         cfg.WalletPassword,
		Certificates: bitumwCert,
	}

	ntfnHandlers := getWalletNtfnHandlers()
	bitumwClient, err := rpcclient.New(connCfgWallet, ntfnHandlers)
	if err != nil {
		log.Errorf("Verify that username and password is correct and that "+
			"rpc.cert is for your wallet: %v", cfg.WalletCert)
		return nil, walletVer, err
	}

	// Ensure the wallet RPC server has a compatible API version.
	ver, err := bitumwClient.Version()
	if err != nil {
		log.Error("Unable to get RPC version: ", err)
		return nil, walletVer, fmt.Errorf("Unable to get node RPC version")
	}

	bitumwVer := ver["bitumwalletjsonrpcapi"]
	walletVer = semver{bitumwVer.Major, bitumwVer.Minor, bitumwVer.Patch}

	if !semverCompatible(requiredWalletAPI, walletVer) {
		log.Warnf("Node JSON-RPC server %v does not have "+
			"a compatible API version. Advertizes %v but require %v",
			cfg.WalletHost, walletVer, requiredWalletAPI)
	}

	return bitumwClient, walletVer, nil
}

func walletGetTickets(ctx *rpcserver.AppContext) (map[chainhash.Hash]string, map[chainhash.Hash]string, error) {
	blockHashToHeightCache := make(map[chainhash.Hash]int32)

	// This is suboptimal to copy and needs fixing.
	userVotingConfig := make(map[string]userdata.UserVotingConfig)
	ctx.RLock()
	for k, v := range ctx.UserVotingConfig {
		userVotingConfig[k] = v
	}
	ctx.RUnlock()

	ignoredLowFeeTickets := make(map[chainhash.Hash]string)
	liveTickets := make(map[chainhash.Hash]string)
	normalFee := 0

	log.Info("Calling GetTickets...")
	timenow := time.Now()
	tickets, err := ctx.WalletConnection.GetTickets(false)
	log.Infof("GetTickets: took %v", time.Since(timenow))

	if err != nil {
		log.Warnf("GetTickets failed: %v", err)
		return ignoredLowFeeTickets, liveTickets, err
	}

	type promise struct {
		rpcclient.FutureGetTransactionResult
	}
	promises := make([]promise, 0, len(tickets))

	log.Debugf("setting up GetTransactionAsync for %v tickets", len(tickets))
	for _, ticket := range tickets {
		// lookup ownership of each ticket
		promises = append(promises, promise{ctx.WalletConnection.GetTransactionAsync(ticket)})
	}

	counter := 0
	for _, p := range promises {
		counter++
		log.Debugf("Receiving GetTransaction result for ticket %v/%v", counter, len(tickets))
		gt, err := p.Receive()
		if err != nil {
			// All tickets should exist and be able to be looked up
			log.Warnf("GetTransaction error: %v", err)
			continue
		}
		for i := range gt.Details {
			addr := gt.Details[i].Address
			_, ok := userVotingConfig[addr]
			if !ok {
				log.Warnf("Could not map ticket %v to a user, user %v doesn't exist", gt.TxID, addr)
				continue
			}

			hash, err := chainhash.NewHashFromStr(gt.TxID)
			if err != nil {
				log.Warnf("invalid ticket %v", err)
				continue
			}

			// All tickets are present in the GetTickets response, whether they
			// pay the correct fee or not.  So we need to verify fees and
			// sort the tickets into their respective maps.
			_, isAdded := ctx.AddedLowFeeTicketsMSA[*hash]
			if isAdded {
				liveTickets[*hash] = userVotingConfig[addr].MultiSigAddress
			} else {

				msgTx, err := rpcserver.MsgTxFromHex(gt.Hex)
				if err != nil {
					log.Warnf("MsgTxFromHex failed for %v: %v", gt.Hex, err)
					continue
				}

				// look up the height at which this ticket was purchased
				var ticketBlockHeight int32
				ticketBlockHash, err := chainhash.NewHashFromStr(gt.BlockHash)
				if err != nil {
					log.Warnf("NewHashFromStr failed for %v: %v", gt.BlockHash, err)
					continue
				}

				height, inCache := blockHashToHeightCache[*ticketBlockHash]
				if inCache {
					ticketBlockHeight = height
				} else {
					gbh, err := ctx.NodeConnection.GetBlockHeader(ticketBlockHash)
					if err != nil {
						log.Warnf("GetBlockHeader failed for %v: %v", ticketBlockHash, err)
						continue
					}

					blockHashToHeightCache[*ticketBlockHash] = int32(gbh.Height)
					ticketBlockHeight = int32(gbh.Height)
				}

				ticketFeesValid, err := ctx.EvaluateStakePoolTicket(msgTx, ticketBlockHeight)
				if ticketFeesValid {
					normalFee++
					liveTickets[*hash] = userVotingConfig[addr].MultiSigAddress
				} else {
					ignoredLowFeeTickets[*hash] = userVotingConfig[addr].MultiSigAddress
					log.Warnf("ignoring ticket %v for msa %v ticketFeesValid %v err %v",
						*hash, ctx.UserVotingConfig[addr].MultiSigAddress, ticketFeesValid, err)
				}
			}
			break
		}
	}

	log.Infof("tickets loaded -- addedLowFee %v ignoredLowFee %v normalFee %v "+
		"live %v total %v", len(ctx.AddedLowFeeTicketsMSA),
		len(ignoredLowFeeTickets), normalFee, len(liveTickets),
		len(tickets))

	return ignoredLowFeeTickets, liveTickets, nil
}

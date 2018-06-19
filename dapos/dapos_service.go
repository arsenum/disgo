/*
 *    This file is part of DAPoS library.
 *
 *    The DAPoS library is free software: you can redistribute it and/or modify
 *    it under the terms of the GNU General Public License as published by
 *    the Free Software Foundation, either version 3 of the License, or
 *    (at your option) any later version.
 *
 *    The DAPoS library is distributed in the hope that it will be useful,
 *    but WITHOUT ANY WARRANTY; without even the implied warranty of
 *    MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 *    GNU General Public License for more details.
 *
 *    You should have received a copy of the GNU General Public License
 *    along with the DAPoS library.  If not, see <http://www.gnu.org/licenses/>.
 */
package dapos

import (
	"math/big"
	"sync"

	"github.com/dgraph-io/badger"
	"github.com/dispatchlabs/disgo/commons/services"

	"os"
	"time"

	"github.com/dispatchlabs/disgo/commons/types"
	"github.com/dispatchlabs/disgo/commons/utils"
	"github.com/dispatchlabs/disgo/disgover"
)

var daposServiceInstance *DAPoSService
var daposServiceOnce sync.Once

type daposEvents struct {
	DAPoSServiceInitFinished string
}

var (
	// Events - `dapos` events
	Events = daposEvents{
		DAPoSServiceInitFinished: "DAPoSServiceInitFinished",
	}
)

// GetDAPoSService
func GetDAPoSService() *DAPoSService {
	daposServiceOnce.Do(func() {
		daposServiceInstance = &DAPoSService{running: false, gossipChan: make(chan *types.Gossip, 1000), transactionChan: make(chan *types.Gossip, 1000)} // TODO: What should this be?
	})
	return daposServiceInstance
}

// DAPoSService -
type DAPoSService struct {
	running         bool
	gossipChan      chan *types.Gossip
	transactionChan chan *types.Gossip
	delegateNodes   []*types.Node // TODO: Should this be here after elections?
}

// IsRunning -
func (this *DAPoSService) IsRunning() bool {
	return this.running
}

// Go -
func (this *DAPoSService) Go(waitGroup *sync.WaitGroup) {
	this.running = true
	utils.Info("running, waiting for delegates sync")

	utils.Events().On(
		disgover.Events.DisGoverServiceInitFinished,
		this.disGoverServiceInitFinished,
	)
}

// OnEvent - Event to
func (this *DAPoSService) disGoverServiceInitFinished() {

	if disgover.GetDisGoverService().ThisNode.Type == types.TypeDelegate {
		this.peerSynchronize()
	}

	// Create genesis transaction.
	err := this.createGenesisTransactionAndAccount()
	if err != nil {
		utils.Fatal("unable to create genesis block", err)
		os.Exit(1)
		return
	}

	go this.gossipWorker()
	go this.transactionWorker()

	utils.Events().Raise(Events.DAPoSServiceInitFinished)
}

// createGenesisTransactionAndAccount
func (this *DAPoSService) createGenesisTransactionAndAccount() error {
	txn := services.GetDb().NewTransaction(true)
	defer txn.Discard()
	transaction, err := types.ToTransactionFromJson([]byte(types.GetConfig().GenesisTransaction))
	if err != nil {
		return err
	}
	_, err = types.ToTransactionByKey(txn, []byte(transaction.Key()))
	if err != nil {
		if err == badger.ErrKeyNotFound {
			err = transaction.Set(txn)
			if err != nil {
				return err
			}
			account := &types.Account{Address: transaction.To, Name: "Dispatch Labs", Balance: big.NewInt(transaction.Value), Updated: time.Now(), Created: time.Now()}
			err = account.Set(txn)
			if err != nil {
				return err
			}
		}
	}
	return txn.Commit(nil)
}
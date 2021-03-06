// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package state provides a caching layer atop the Ethereum state trie.
package state

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/dispatchlabs/disgo/commons/crypto"
	"github.com/dispatchlabs/disgo/commons/services"
	dispatTypes "github.com/dispatchlabs/disgo/commons/types"
	"github.com/dispatchlabs/disgo/commons/utils"
	"github.com/dispatchlabs/disgo/dvm/ethereum/common"
	"github.com/dispatchlabs/disgo/dvm/ethereum/log"
	"github.com/dispatchlabs/disgo/dvm/ethereum/rlp"
	"github.com/dispatchlabs/disgo/dvm/ethereum/trie"
	"github.com/dispatchlabs/disgo/dvm/ethereum/types"
)

type revision struct {
	id           int
	journalIndex int
}

var (
	// emptyState is the known hash of an empty state trie entry.
	emptyState = crypto.NewHash(nil)

	// emptyCode is the known hash of the empty EVM bytecode.
	emptyCode = crypto.NewHash(nil)
)

// StateDBs within the ethereum protocol are used to store anything
// within the merkle trie. StateDBs take care of caching and storing
// nested states. It's the general query interface to retrieve:
// * Contracts
// * Accounts
type StateDB struct {
	db   Database
	trie Trie

	// This map holds 'live' objects, which will get modified while processing a state transition.
	StateObjects      map[crypto.AddressBytes]*stateObject
	stateObjectsDirty map[crypto.AddressBytes]struct{}

	// DB error.
	// State objects are used by the consensus core and VM which are
	// unable to deal with database-level errors. Any error that occurs
	// during a database read is memoized here and will eventually be returned
	// by StateDB.Commit.
	dbErr error

	// The refund counter, also used by state transitioning.
	refund uint64

	thash, bhash crypto.HashBytes
	txIndex      int
	logs         map[crypto.HashBytes][]*types.Log
	logSize      uint

	preimages map[crypto.HashBytes][]byte

	// Journal of state modifications. This is the backbone of
	// Snapshot and RevertToSnapshot.
	journal        *journal
	validRevisions []revision
	nextRevisionId int

	lock sync.Mutex
}

// Create a new state from a given trie.
func New(root crypto.HashBytes, db Database) (*StateDB, error) {
	tr, err := db.OpenTrie(root)
	if err != nil {
		return nil, err
	}
	return &StateDB{
		db:                db,
		trie:              tr,
		StateObjects:      make(map[crypto.AddressBytes]*stateObject),
		stateObjectsDirty: make(map[crypto.AddressBytes]struct{}),
		logs:              make(map[crypto.HashBytes][]*types.Log),
		preimages:         make(map[crypto.HashBytes][]byte),
		journal:           newJournal(),
	}, nil
}

// setError remembers the first non-nil error it is called with.
func (self *StateDB) setError(err error) {
	utils.Debug(fmt.Sprintf("StateDB-setError: %v", err))

	if self.dbErr == nil {
		self.dbErr = err
	}
}

func (self *StateDB) Error() error {
	return self.dbErr
}

// Reset clears out all ephemeral state objects from the state db, but keeps
// the underlying state trie to avoid reloading data for the next operations.
func (self *StateDB) Reset(root crypto.HashBytes) error {
	utils.Debug(fmt.Sprintf("StateDB-Reset: %s", crypto.EncodeNo0x(root[:])))

	tr, err := self.db.OpenTrie(root)
	if err != nil {
		return err
	}
	self.trie = tr
	self.StateObjects = make(map[crypto.AddressBytes]*stateObject)
	self.stateObjectsDirty = make(map[crypto.AddressBytes]struct{})
	self.thash = crypto.HashBytes{}
	self.bhash = crypto.HashBytes{}
	self.txIndex = 0
	self.logs = make(map[crypto.HashBytes][]*types.Log)
	self.logSize = 0
	self.preimages = make(map[crypto.HashBytes][]byte)
	self.clearJournalAndRefund()
	return nil
}

func (self *StateDB) AddLog(log *types.Log) {
	utils.Debug(fmt.Sprintf("StateDB-AddLog: %v", log))

	self.journal.append(addLogChange{txhash: self.thash})

	log.TxHash = self.thash
	log.BlockHash = self.bhash
	log.TxIndex = uint(self.txIndex)
	log.Index = self.logSize
	self.logs[self.thash] = append(self.logs[self.thash], log)
	self.logSize++
}

func (self *StateDB) GetLogs(hash crypto.HashBytes) []*types.Log {
	utils.Debug(fmt.Sprintf("StateDB-GetLogs: %s", crypto.EncodeNo0x(hash[:])))

	return self.logs[hash]
}

func (self *StateDB) Logs() []*types.Log {
	var logs []*types.Log
	for _, lgs := range self.logs {
		logs = append(logs, lgs...)
	}
	return logs
}

// AddPreimage records a SHA3 preimage seen by the VM.
func (self *StateDB) AddPreimage(hash crypto.HashBytes, preimage []byte) {
	utils.Debug(fmt.Sprintf("StateDB-AddPreimage: %s", crypto.EncodeNo0x(hash[:])))

	if _, ok := self.preimages[hash]; !ok {
		self.journal.append(addPreimageChange{hash: hash})
		pi := make([]byte, len(preimage))
		copy(pi, preimage)
		self.preimages[hash] = pi
	}
}

// Preimages returns a list of SHA3 preimages that have been submitted.
func (self *StateDB) Preimages() map[crypto.HashBytes][]byte {
	return self.preimages
}

func (self *StateDB) AddRefund(gas uint64) {
	utils.Debug(fmt.Sprintf("StateDB-AddRefund: %v", gas))

	self.journal.append(refundChange{prev: self.refund})
	self.refund += gas
}

// Exist reports whether the given account address exists in the state.
// Notably this also returns true for suicided accounts.
func (self *StateDB) Exist(addr crypto.AddressBytes) bool {
	utils.Debug(fmt.Sprintf("StateDB-Exist: %s", crypto.EncodeNo0x(addr[:])))

	return self.getStateObject(addr) != nil
}

// Empty returns whether the state object is either non-existent
// or empty according to the EIP161 specification (balance = nonce = code = 0)
func (self *StateDB) Empty(addr crypto.AddressBytes) bool {
	utils.Debug(fmt.Sprintf("StateDB-Empty: %s", crypto.EncodeNo0x(addr[:])))

	so := self.getStateObject(addr)
	return so == nil || so.empty()
}

// Retrieve the balance from the given address or 0 if object not found
func (self *StateDB) GetBalance(addr crypto.AddressBytes) *big.Int {
	utils.Debug(fmt.Sprintf("StateDB-GetBalance: %s", crypto.Encode(addr[:])))

	stateObject := self.getStateObject(addr)
	if stateObject != nil {
		// utils.Debug(fmt.Sprintf("%s : %d @ %s", common.EthAddressToDispatchAddress(addr), stateObject.Balance(), utils.GetCallStackWithFileAndLineNumber()))

		return stateObject.account.Balance
	}
	return common.Big0
}

func (self *StateDB) GetNonce(addr crypto.AddressBytes) uint64 {
	utils.Debug(fmt.Sprintf("StateDB-GetNonce: %s", crypto.EncodeNo0x(addr[:])))

	stateObject := self.getStateObject(addr)
	if stateObject != nil {
		return stateObject.account.Nonce
	}

	return 0
}

func (self *StateDB) GetCode(addr crypto.AddressBytes) []byte {
	utils.Debug(fmt.Sprintf("StateDB-GetCode: %s", crypto.EncodeNo0x(addr[:])))

	stateObject := self.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Code(self.db)
	}
	return nil
}

func (self *StateDB) GetCodeSize(addr crypto.AddressBytes) int {
	utils.Debug(fmt.Sprintf("StateDB-GetCodeSize: %s", crypto.EncodeNo0x(addr[:])))

	stateObject := self.getStateObject(addr)
	if stateObject == nil {
		return 0
	}
	if stateObject.code != nil {
		return len(stateObject.code)
	}

	var addressAsBytes = crypto.GetAddressBytes(stateObject.account.Address)
	var addressHash = crypto.NewHash(addressAsBytes[:])

	size, err := self.db.ContractCodeSize(addressHash, crypto.BytesToHash(stateObject.account.CodeHash))
	if err != nil {
		self.setError(err)
	}

	utils.Debug(fmt.Sprintf("StateDB-GetCodeSize: %v", size))

	return size
}

func (self *StateDB) GetCodeHash(addr crypto.AddressBytes) crypto.HashBytes {
	utils.Debug(fmt.Sprintf("StateDB-GetCodeHash: %s", crypto.EncodeNo0x(addr[:])))

	stateObject := self.getStateObject(addr)
	if stateObject == nil {
		return crypto.HashBytes{}
	}
	return crypto.BytesToHash(stateObject.account.CodeHash)
}

func (self *StateDB) GetState(addr crypto.AddressBytes, bhash crypto.HashBytes) crypto.HashBytes {
	utils.Debug(fmt.Sprintf("StateDB-GetState: %s", crypto.EncodeNo0x(addr[:])))

	stateObject := self.getStateObject(addr)
	if stateObject != nil {
		return stateObject.GetState(self.db, bhash)
	}
	return crypto.HashBytes{}
}

// Database retrieves the low level database supporting the lower level trie ops.
func (self *StateDB) Database() Database {
	return self.db
}

// StorageTrie returns the storage trie of an account.
// The return value is a copy and is nil for non-existent accounts.
func (self *StateDB) StorageTrie(addr crypto.AddressBytes) Trie {
	utils.Debug(fmt.Sprintf("StateDB-StorageTrie: %s", crypto.EncodeNo0x(addr[:])))

	stateObject := self.getStateObject(addr)
	if stateObject == nil {
		return nil
	}
	cpy := stateObject.deepCopy(self)
	return cpy.updateTrie(self.db)
}

func (self *StateDB) HasSuicided(addr crypto.AddressBytes) bool {
	utils.Debug(fmt.Sprintf("StateDB-HasSuicided: %s", crypto.EncodeNo0x(addr[:])))

	stateObject := self.getStateObject(addr)
	if stateObject != nil {
		return stateObject.suicided
	}
	return false
}

/*
 * SETTERS
 */

// AddBalance adds amount to the account associated with addr.
func (self *StateDB) AddBalance(addr crypto.AddressBytes, amount *big.Int) {
	utils.Debug(fmt.Sprintf("StateDB-AddBalance: %s", crypto.EncodeNo0x(addr[:])))

	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.AddBalance(amount)
		self.GetBalance(addr)
	}
}

// SubBalance subtracts amount from the account associated with addr.
func (self *StateDB) SubBalance(addr crypto.AddressBytes, amount *big.Int) {
	utils.Debug(fmt.Sprintf("StateDB-SubBalance: %s", crypto.EncodeNo0x(addr[:])))

	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SubBalance(amount)
		self.GetBalance(addr)

	}
}

func (self *StateDB) SetBalance(addr crypto.AddressBytes, amount *big.Int) {
	utils.Debug(fmt.Sprintf("StateDB-SetBalance: %s", crypto.EncodeNo0x(addr[:])))

	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetBalance(amount)
	}
}

func (self *StateDB) SetNonce(addr crypto.AddressBytes, nonce uint64) {
	utils.Debug(fmt.Sprintf("StateDB-SetNonce: %s", crypto.EncodeNo0x(addr[:])))

	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetNonce(nonce)
	}
}

func (self *StateDB) SetCode(addr crypto.AddressBytes, code []byte) {
	utils.Debug(fmt.Sprintf("StateDB-SetCode: %s", crypto.EncodeNo0x(addr[:])))

	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetCode(crypto.NewHash(code), code)
	}
}

func (self *StateDB) SetState(addr crypto.AddressBytes, key, value crypto.HashBytes) {
	utils.Debug(fmt.Sprintf("StateDB-SetState: %s", crypto.EncodeNo0x(addr[:])))

	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetState(self.db, key, value)
	}
}

// Suicide marks the given account as suicided.
// This clears the account balance.
//
// The account's state object is still available until the state is committed,
// getStateObject will return a non-nil account after Suicide.
func (self *StateDB) Suicide(addr crypto.AddressBytes) bool {
	utils.Debug(fmt.Sprintf("StateDB-Suicide: %s", crypto.EncodeNo0x(addr[:])))

	stateObject := self.getStateObject(addr)
	if stateObject == nil {
		return false
	}

	self.journal.append(suicideChange{
		account:     addr,
		prev:        stateObject.suicided,
		prevbalance: new(big.Int).Set(stateObject.account.Balance),
	})
	stateObject.markSuicided()
	stateObject.account.Balance = big.NewInt(0)

	return true
}

//
// Setting, updating & deleting state object methods.
//

// updateStateObject writes the given object to the trie.
func (self *StateDB) updateStateObject(stateObject *stateObject) {
	utils.Debug(fmt.Sprintf("StateDB-updateStateObject:"))

	var addressAsBytes = crypto.GetAddressBytes(stateObject.account.Address)
	data, err := rlp.EncodeToBytes(stateObject)
	if err != nil {
		panic(fmt.Errorf("can't encode object at %x: %v", addressAsBytes[:], err))
	}
	//if len(stateObject.code) > 0 {
	//	fmt.Printf("This is where you want your breakpoint")
	//}
	self.setError(self.trie.TryUpdate(addressAsBytes[:], data))
}

// deleteStateObject removes the given object from the state trie.
func (self *StateDB) deleteStateObject(stateObject *stateObject) {
	utils.Debug(fmt.Sprintf("StateDB-deleteStateObject:"))

	stateObject.deleted = true

	var addressAsBytes = crypto.GetAddressBytes(stateObject.account.Address)
	self.setError(self.trie.TryDelete(addressAsBytes[:]))
}

// Retrieve a state object given my the address. Returns nil if not found.
func (self *StateDB) getStateObject(addr crypto.AddressBytes) (stateObject *stateObject) {
	utils.Debug(fmt.Sprintf("StateDB-getStateObject: %s", crypto.EncodeNo0x(addr[:])))

	// Prefer 'live' objects.
	if obj := self.StateObjects[addr]; obj != nil {
		if obj.deleted {
			return nil
		}
		return obj
	}

	// Load the object from the database.
	enc, err := self.trie.TryGet(addr[:])
	if len(enc) == 0 {
		self.setError(err)
		return nil
	}
	var data dispatTypes.Account
	if err := rlp.DecodeBytes(enc, &data); err != nil {
		log.Error("Failed to decode state object", "addr", addr, "err", err)
		return nil
	}
	// Insert into the live set.
	obj := newStateObject(self, addr, data)
	self.setStateObject(obj)
	return obj
}

func (self *StateDB) setStateObject(object *stateObject) {
	utils.Debug(fmt.Sprintf("StateDB-setStateObject:"))

	self.lock.Lock()
	defer self.lock.Unlock()

	var addressAsBytes = crypto.GetAddressBytes(object.account.Address)

	self.StateObjects[addressAsBytes] = object
}

// Retrieve a state object or create a new state object if nil.
func (self *StateDB) GetOrNewStateObject(addr crypto.AddressBytes) *stateObject {
	utils.Debug(fmt.Sprintf("StateDB-GetOrNewStateObject: %s", crypto.EncodeNo0x(addr[:])))

	stateObject := self.getStateObject(addr)
	if stateObject == nil || stateObject.deleted {
		stateObject, _ = self.createObject(addr)
	}
	return stateObject
}

// createObject creates a new state object. If there is an existing account with
// the given address, it is overwritten and returned as the second return value.
func (self *StateDB) createObject(addr crypto.AddressBytes) (newobj, prev *stateObject) {
	addressAsString := hex.EncodeToString(addr.Bytes())

	utils.Debug(fmt.Sprintf("StateDB-createObject: %s", addressAsString))

	now := time.Now()
	var account = dispatTypes.Account{
		Address: addressAsString,
		Balance: common.Big0,
		Updated: now,
		Created: now,
	}

	prev = self.getStateObject(addr)
	if prev == nil {
		// BadgerDatabase-look for Existing Account in Badger
		txn := services.NewTxn(false)
		defer txn.Discard()
		accountFromBadger, accountFromBadgerErr := dispatTypes.ToAccountByAddress(txn, addressAsString)

		if accountFromBadgerErr == nil {
			account = *accountFromBadger
		}
	}

	newobj = newStateObject(self, addr, account)

	if prev == nil {
		self.journal.append(createObjectChange{account: addr})
	} else {
		self.journal.append(resetObjectChange{prev: prev})
	}

	self.setStateObject(newobj)
	return newobj, prev
}

// CreateAccount explicitly creates a state object. If a state object with the address
// already exists the balance is carried over to the new account.
//
// CreateAccount is called during the EVM CREATE operation. The situation might arise that
// a contract does the following:
//
//   1. sends funds to sha(account ++ (nonce + 1))
//   2. tx_create(sha(account ++ nonce)) (note that this gets the address of 1)
//
// Carrying over the balance ensures that Ether doesn't disappear.
func (self *StateDB) CreateAccount(addr crypto.AddressBytes) {
	utils.Debug(fmt.Sprintf("StateDB-CreateAccount: %s", crypto.EncodeNo0x(addr[:])))

	new, prev := self.createObject(addr)
	if prev != nil {
		new.setBalance(prev.account.Balance)
	}
}

func (db *StateDB) ForEachStorage(addr crypto.AddressBytes, cb func(key, value crypto.HashBytes) bool) {
	utils.Debug(fmt.Sprintf("StateDB-ForEachStorage: %s", crypto.EncodeNo0x(addr[:])))

	so := db.getStateObject(addr)
	if so == nil {
		return
	}

	// When iterating over the storage check the cache first
	for h, value := range so.cachedStorage {
		cb(h, value)
	}

	it := trie.NewIterator(so.getTrie(db.db).NodeIterator(nil))
	for it.Next() {
		// ignore cached values
		key := crypto.BytesToHash(db.trie.GetKey(it.Key))
		if _, ok := so.cachedStorage[key]; !ok {
			cb(key, crypto.BytesToHash(it.Value))
		}
	}
}

// Copy creates a deep, independent copy of the state.
// Snapshots of the copied state cannot be applied to the copy.
func (self *StateDB) Copy() *StateDB {
	utils.Debug(fmt.Sprintf("StateDB-Copy:"))

	self.lock.Lock()
	defer self.lock.Unlock()

	// Copy all the basic fields, initialize the memory ones
	state := &StateDB{
		db:                self.db,
		trie:              self.db.CopyTrie(self.trie),
		StateObjects:      make(map[crypto.AddressBytes]*stateObject, len(self.journal.dirties)),
		stateObjectsDirty: make(map[crypto.AddressBytes]struct{}, len(self.journal.dirties)),
		refund:            self.refund,
		logs:              make(map[crypto.HashBytes][]*types.Log, len(self.logs)),
		logSize:           self.logSize,
		preimages:         make(map[crypto.HashBytes][]byte),
		journal:           newJournal(),
	}
	// Copy the dirty states, logs, and preimages
	for addr := range self.journal.dirties {
		state.StateObjects[addr] = self.StateObjects[addr].deepCopy(state)
		state.stateObjectsDirty[addr] = struct{}{}
	}
	// Above, we don't copy the actual journal. This means that if the copy is copied, the
	// loop above will be a no-op, since the copy's journal is empty.
	// Thus, here we iterate over StateObjects, to enable copies of copies
	for addr := range self.stateObjectsDirty {
		if _, exist := state.StateObjects[addr]; !exist {
			state.StateObjects[addr] = self.StateObjects[addr].deepCopy(state)
			state.stateObjectsDirty[addr] = struct{}{}
		}
	}

	for hash, logs := range self.logs {
		state.logs[hash] = make([]*types.Log, len(logs))
		copy(state.logs[hash], logs)
	}
	for hash, preimage := range self.preimages {
		state.preimages[hash] = preimage
	}
	return state
}

// Snapshot returns an identifier for the current revision of the state.
func (self *StateDB) Snapshot() int {
	utils.Debug(fmt.Sprintf("StateDB-Snapshot:"))

	id := self.nextRevisionId
	self.nextRevisionId++
	self.validRevisions = append(self.validRevisions, revision{id, self.journal.length()})
	return id
}

// RevertToSnapshot reverts all state changes made since the given revision.
func (self *StateDB) RevertToSnapshot(revid int) {
	utils.Debug(fmt.Sprintf("StateDB-RevertToSnapshot:"))

	// Find the snapshot in the stack of valid snapshots.
	idx := sort.Search(len(self.validRevisions), func(i int) bool {
		return self.validRevisions[i].id >= revid
	})
	if idx == len(self.validRevisions) || self.validRevisions[idx].id != revid {
		panic(fmt.Errorf("revision id %v cannot be reverted", revid))
	}
	snapshot := self.validRevisions[idx].journalIndex

	// Replay the journal to undo changes and remove invalidated snapshots
	self.journal.revert(self, snapshot)
	self.validRevisions = self.validRevisions[:idx]
}

// GetRefund returns the current value of the refund counter.
func (self *StateDB) GetRefund() uint64 {
	utils.Debug(fmt.Sprintf("StateDB-GetRefund:"))

	return self.refund
}

// Finalise finalises the state by removing the self destructed objects
// and clears the journal as well as the refunds.
func (s *StateDB) Finalise(deleteEmptyObjects bool) {
	utils.Debug(fmt.Sprintf("StateDB-Finalise:"))

	for addr := range s.journal.dirties {
		stateObject, exist := s.StateObjects[addr]
		if !exist {
			// ripeMD is 'touched' at block 1714175, in tx 0x1237f737031e40bcde4a8b7e717b2d15e3ecadfe49bb1bbc71ee9deb09c6fcf2
			// That tx goes out of gas, and although the notion of 'touched' does not exist there, the
			// touch-event will still be recorded in the journal. Since ripeMD is a special snowflake,
			// it will persist in the journal even though the journal is reverted. In this special circumstance,
			// it may exist in `s.journal.dirties` but not in `s.StateObjects`.
			// Thus, we can safely ignore it here
			continue
		}

		if stateObject.suicided || (deleteEmptyObjects && stateObject.empty()) {
			s.deleteStateObject(stateObject)
		} else {
			stateObject.updateRoot(s.db)
			s.updateStateObject(stateObject)
		}
		s.stateObjectsDirty[addr] = struct{}{}
	}
	// Invalidate journal because reverting across transactions is not allowed.
	s.clearJournalAndRefund()
}

// IntermediateRoot computes the current root hash of the state trie.
// It is called in between transactions to get the root hash that
// goes into transaction receipts.
func (s *StateDB) IntermediateRoot(deleteEmptyObjects bool) crypto.HashBytes {
	utils.Debug(fmt.Sprintf("StateDB-IntermediateRoot:"))

	s.lock.Lock()
	defer s.lock.Unlock()

	s.Finalise(deleteEmptyObjects)
	return s.trie.Hash()
}

// Prepare sets the current transaction hash and index and block hash which is
// used when the EVM emits new state logs.
func (self *StateDB) Prepare(thash, bhash crypto.HashBytes, ti int) {
	utils.Debug(fmt.Sprintf("StateDB-Prepare:"))

	self.thash = thash
	self.bhash = bhash
	self.txIndex = ti
}

// DeleteSuicides flags the suicided objects for deletion so that it
// won't be referenced again when called / queried up on.
//
// DeleteSuicides should not be used for consensus related updates
// under any circumstances.
func (s *StateDB) DeleteSuicides() {
	utils.Debug(fmt.Sprintf("StateDB-DeleteSuicides:"))

	// Reset refund so that any used-gas calculations can use this method.
	s.clearJournalAndRefund()

	for addr := range s.stateObjectsDirty {
		stateObject := s.StateObjects[addr]

		// If the object has been removed by a suicide
		// flag the object as deleted.
		if stateObject.suicided {
			stateObject.deleted = true
		}
		delete(s.stateObjectsDirty, addr)
	}
}

func (s *StateDB) clearJournalAndRefund() {
	utils.Debug(fmt.Sprintf("StateDB-clearJournalAndRefund:"))

	s.journal = newJournal()
	s.validRevisions = s.validRevisions[:0]
	s.refund = 0
}

// Commit writes the state to the underlying in-memory trie database.
func (s *StateDB) Commit(deleteEmptyObjects bool) (root crypto.HashBytes, err error) {
	utils.Debug(fmt.Sprintf("StateDB-Commit:"))

	defer s.clearJournalAndRefund()

	s.lock.Lock()
	defer s.lock.Unlock()

	for addr := range s.journal.dirties {
		s.stateObjectsDirty[addr] = struct{}{}
	}
	// Commit objects to the trie.
	for addr, stateObject := range s.StateObjects {
		_, isDirty := s.stateObjectsDirty[addr]
		switch {
		case stateObject.suicided || (isDirty && deleteEmptyObjects && stateObject.empty()):
			// If the object has been removed, don't bother syncing it
			// and just mark it for deletion in the trie.
			s.deleteStateObject(stateObject)
		case isDirty:
			// Write any contract code associated with the state object
			if stateObject.code != nil && stateObject.dirtyCode {
				s.db.TrieDB().InsertBlob(crypto.BytesToHash(stateObject.account.CodeHash), stateObject.code)
				stateObject.dirtyCode = false
			}
			// Write any storage changes in the state object to its storage trie.
			if err := stateObject.CommitTrie(s.db); err != nil {
				return crypto.HashBytes{}, err
			}
			// Update the object in the main account trie.
			s.updateStateObject(stateObject)
		}
		delete(s.stateObjectsDirty, addr)
	}
	// Write trie changes.
	root, err = s.trie.Commit(func(leaf []byte, parent crypto.HashBytes) error {
		var account dispatTypes.Account
		if err := rlp.DecodeBytes(leaf, &account); err != nil {
			return nil
		}
		if account.Root != emptyState {
			s.db.TrieDB().Reference(account.Root, parent)
		}
		code := crypto.BytesToHash(account.CodeHash)
		if code != emptyCode {
			s.db.TrieDB().Reference(code, parent)
		}
		return nil
	})

	log.Debug("Trie cache stats after commit", "misses", trie.CacheMisses(), "unloads", trie.CacheUnloads())
	return root, err
}

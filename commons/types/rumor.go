/*
 *    This file is part of Disgo-Commons library.
 *
 *    The Disgo-Commons library is free software: you can redistribute it and/or modify
 *    it under the terms of the GNU General Public License as published by
 *    the Free Software Foundation, either version 3 of the License, or
 *    (at your option) any later version.
 *
 *    The Disgo-Commons library is distributed in the hope that it will be useful,
 *    but WITHOUT ANY WARRANTY; without even the implied warranty of
 *    MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 *    GNU General Public License for more details.
 *
 *    You should have received a copy of the GNU General Public License
 *    along with the Disgo-Commons library.  If not, see <http://www.gnu.org/licenses/>.
 */
package types

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"github.com/dispatchlabs/disgo/commons/crypto"
	"github.com/dispatchlabs/disgo/commons/utils"
	"time"
	"sort"
	"fmt"
)

// Rumor
type Rumor struct {
	Hash            string // Hash = (Address + TransactionHash + Time)
	Address         string
	TransactionHash string
	Time            int64
	Signature       string
}

// UnmarshalJSON
func (this *Rumor) UnmarshalJSON(bytes []byte) error {
	var jsonMap map[string]interface{}
	error := json.Unmarshal(bytes, &jsonMap)
	if error != nil {
		return error
	}
	if jsonMap["hash"] != nil {
		this.Hash = jsonMap["hash"].(string)
	}
	if jsonMap["address"] != nil {
		this.Address = jsonMap["address"].(string)
	}
	if jsonMap["transactionHash"] != nil {
		this.TransactionHash = jsonMap["transactionHash"].(string)
	}
	if jsonMap["time"] != nil {
		this.Time = int64(jsonMap["time"].(float64))
	}
	if jsonMap["signature"] != nil {
		this.Signature = jsonMap["signature"].(string)
	}
	return nil
}

// MarshalJSON
func (this Rumor) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Hash            string `json:"hash"`
		Address         string `json:"address"`
		TransactionHash string `json:"transactionHash"`
		Time            int64  `json:"time"`
		Signature       string `json:"signature"`
	}{
		Hash:            this.Hash,
		Address:         this.Address,
		TransactionHash: this.TransactionHash,
		Time:            this.Time,
		Signature:       this.Signature,
	})
}

// String
func (this Rumor) String() string {
	bytes, err := json.Marshal(this)
	if err != nil {
		utils.Error("unable to marshal rumor", err)
		return ""
	}
	return string(bytes)
}

// NewHash
func (this Rumor) NewHash() string {
	addressBytes, err := hex.DecodeString(this.Address)
	if err != nil {
		utils.Error("unable to decode address", err)
		return ""
	}
	transactionHashBytes, err := hex.DecodeString(this.TransactionHash)
	if err != nil {
		utils.Error("unable to decode transaction", err)
		return ""
	}
	var values = []interface{}{
		addressBytes,
		transactionHashBytes,
		this.Time,
	}
	buffer := new(bytes.Buffer)
	for _, value := range values {
		err := binary.Write(buffer, binary.LittleEndian, value)
		if err != nil {
			utils.Fatal("unable to write rumor bytes to buffer", err)
			return ""
		}
	}
	delegateHash := crypto.NewHash(buffer.Bytes())
	return hex.EncodeToString(delegateHash[:])
}

// Verify
func (this Rumor) Verify() bool {
	if len(this.Hash) != crypto.HashLength*2 {
		return false
	}
	if len(this.Address) != crypto.AddressLength*2 {
		return false
	}
	if len(this.TransactionHash) != crypto.HashLength*2 {
		return false
	}
	if len(this.Signature) != crypto.SignatureLength*2 {
		return false
	}

	// Hash ok?
	if this.Hash != this.NewHash() {
		return false
	}
	hashBytes, err := hex.DecodeString(this.Hash)
	if err != nil {
		utils.Error("unable to decode hash", err)
		return false
	}
	signatureBytes, err := hex.DecodeString(this.Signature)
	if err != nil {
		utils.Error("unable to decode signature", err)
		return false
	}
	publicKeyBytes, err := crypto.ToPublicKey(hashBytes, signatureBytes)
	if err != nil {
		return false
	}

	// Derived address from publicKeyBytes match address?
	address := hex.EncodeToString(crypto.ToAddress(publicKeyBytes))
	if address != this.Address {
		return false
	}
	return crypto.VerifySignature(publicKeyBytes, hashBytes, signatureBytes)
}

// ToJsonByRumors
func ToJsonByRumors(rumors []*Rumor) ([]byte, error) {
	bytes, err := json.Marshal(rumors)
	if err != nil {
		return nil, err
	}
	return bytes, nil
}

// ToRumorFromJson -
func ToRumorFromJson(payload []byte) (*Rumor, error) {
	rumor := &Rumor{}
	err := json.Unmarshal(payload, rumor)
	if err != nil {
		return nil, err
	}
	return rumor, nil
}

// ToRumorsFromJson -
func ToRumorsFromJson(payload []byte) ([]*Rumor, error) {
	var rumors = make([]*Rumor, 0)
	err := json.Unmarshal(payload, &rumors)
	if err != nil {
		return nil, err
	}
	return rumors, nil
}

// NewRumor -
func NewRumor(privateKey string, address string, transactionHash string) *Rumor {
	rumor := &Rumor{}
	rumor.Address = address
	rumor.TransactionHash = transactionHash
	rumor.Time = utils.ToMilliSeconds(time.Now())
	rumor.Hash = rumor.NewHash()
	privateKeyBytes, err := hex.DecodeString(privateKey)
	if err != nil {
		utils.Error("unable to decode privateKey", err)
		return nil
	}
	hashBytes, err := hex.DecodeString(rumor.Hash)
	if err != nil {
		utils.Error("unable to decode hash", err)
		return nil
	}
	signature, err := crypto.NewSignature(privateKeyBytes, hashBytes)
	if err != nil {
		utils.Error(err.Error())
		return nil
	}

	rumor.Signature = hex.EncodeToString(signature)
	return rumor
}


type RumorsSorter struct {
	Rumors  []Rumor
}

// Len is part of sort.Interface.
func (this RumorsSorter) Len() int {
	return len(this.Rumors)
}

// Swap is part of sort.Interface.
func (this RumorsSorter) Swap(i, j int) {
	this.Rumors[i], this.Rumors[j] = this.Rumors[j], this.Rumors[i]
}

// Less is part of sort.Interface. It is implemented by calling the "by" closure in the sorter.
func (this RumorsSorter) Less(i, j int) bool {
	return this.Rumors[i].Time < this.Rumors[j].Time
}

func ValidateTimeDelta(rumors []Rumor) bool {
	result := true
	rumorSorter := RumorsSorter{rumors}
	sort.Sort(rumorSorter)
	len := rumorSorter.Len()

	timing := make([]int64, 0)
	now := utils.ToMilliSeconds(time.Now())
	initialTime := now - rumorSorter.Rumors[len-1].Time
	timing = append(timing, initialTime)

	if  now - rumorSorter.Rumors[len-1].Time > GossipTimeout {
		msg := fmt.Sprintf("gossip for [hash=%s] to local delegate [adresss=%s] took [time=%v]", rumorSorter.Rumors[len-1].TransactionHash, rumorSorter.Rumors[len-1].Address, initialTime)
		utils.Info(msg)
		result = false
	}
	if len > 1 {
		for i := 1; i < len; i++ {
			gossipTime := rumorSorter.Rumors[i].Time - rumorSorter.Rumors[i-1].Time
			timing = append(timing, gossipTime)
			if gossipTime > GossipTimeout {
				msg := fmt.Sprintf("gossip for [hash=%s] between delegate [adresss=%s] and delegage [adresss=%s] took [time=%v]", rumorSorter.Rumors[i].TransactionHash, rumorSorter.Rumors[i].Address, rumorSorter.Rumors[i-1].Address, gossipTime)
				utils.Warn(msg)
				result = false
			}
		}
	}
	return result
}

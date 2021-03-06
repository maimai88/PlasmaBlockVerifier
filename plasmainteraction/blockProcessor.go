package plasmainteraction

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log"
	"math/big"
	"sync"

	"github.com/dgraph-io/badger"
	common "github.com/ethereum/go-ethereum/common"
	"github.com/matterinc/PlasmaBlockVerifier/messageStructures"
	"github.com/matterinc/PlasmaCommons/block"
	"github.com/matterinc/PlasmaCommons/transaction"
)

var (
	UtxoIndexPrefix      = []byte("utxo")
	BlockNumberKey       = []byte("blockNumber")
	TransactionNumberKey = []byte("txNumber")
	SpendingIndexKey     = []byte("spend")
)

type TransactionPayload struct {
	txNumber uint32
	tx       *transaction.SignedTransaction
}

type PreprocessedTransactionPayload struct {
	txNumber               uint32
	keysToDelete           [][]byte
	keysToWrite            [][]byte
	spendingIndexesToWrite [][2][]byte
	depositCheckoutRequest *messageStructures.DepositIndexCheckoutRequest
}

type ResultPayload struct {
	TxNumber                 uint32
	Result                   bool
	NonrecoverableError      error
	DepositIndexRequest      *messageStructures.DepositIndexCheckoutRequest
	WithdrawChallengeRequest *messageStructures.WithdrawChallengeRequest
}

type BlockProcessor struct {
	db                 *badger.DB
	concurrencyLimit   int
	sliceSize          int
	concurrencyChannel chan bool
	resultsChannel     chan bool
}

func NewBlockProcessor(db *badger.DB, concurrencyLimit int, sliceSize int) *BlockProcessor {
	concurrencyChannel := make(chan bool, concurrencyLimit)
	resultsChannel := make(chan bool, concurrencyLimit)
	newInstance := &BlockProcessor{db, concurrencyLimit, sliceSize, concurrencyChannel, resultsChannel}
	return newInstance
}

func (p *BlockProcessor) ValidateBlock(blockBytes []byte, expectedHeaderHash []byte, expectedMerkleRoot []byte) (*block.Block, error) {
	parsedBlock, err := block.NewBlockFromBytes(blockBytes)
	if err != nil {
		return nil, err
	}
	err = parsedBlock.Validate()
	if err != nil {
		return nil, err
	}
	blockHeaderHash, err := parsedBlock.BlockHeader.GetHash()
	if err != nil {
		return nil, err
	}
	if bytes.Compare(expectedHeaderHash, blockHeaderHash[:]) != 0 {
		// return nil, errors.New("Header hash mismatch")
	}
	merkleRootHash := parsedBlock.BlockHeader.MerkleTreeRoot
	if bytes.Compare(expectedMerkleRoot, merkleRootHash[:]) != 0 {
		return nil, errors.New("Merkle root mismatch")
	}
	return parsedBlock, nil
}

func (p *BlockProcessor) ProcessBlock(blockBytes, expectedHeaderHash, expectedMerkleRoot []byte) ([]*messageStructures.DepositIndexCheckoutRequest, []*messageStructures.WithdrawChallengeRequest, error) {
	parsedBlock, err := p.ValidateBlock(blockBytes, expectedHeaderHash, expectedMerkleRoot)
	if err != nil {
		return nil, nil, err
	}
	return p.ProcessParsedBlock(parsedBlock)
}

func (p *BlockProcessor) ProcessBlockWithoutCommitment(blockBytes []byte) ([]*messageStructures.DepositIndexCheckoutRequest, []*messageStructures.WithdrawChallengeRequest, error) {
	parsedBlock, err := block.NewBlockFromBytes(blockBytes)
	if err != nil {
		return nil, nil, err
	}
	err = parsedBlock.Validate()
	if err != nil {
		return nil, nil, err
	}
	return p.ProcessParsedBlock(parsedBlock)
}

func (p *BlockProcessor) ProcessParsedBlock(parsedBlock *block.Block) ([]*messageStructures.DepositIndexCheckoutRequest, []*messageStructures.WithdrawChallengeRequest, error) {
	blockNumberBytes := parsedBlock.BlockHeader.BlockNumber[:]
	blockNumber := binary.BigEndian.Uint32(blockNumberBytes)
	numTransactions := len(parsedBlock.Transactions)
	sliceSize := p.sliceSize
	numSlices := numTransactions / sliceSize
	if numTransactions%sliceSize != 0 {
		numSlices++
	}

	resChannels := make([]<-chan []ResultPayload, numSlices)
	for i := 0; i < numSlices; i++ {
		minTxNumber := uint32(0)
		maxTxNumber := uint32(0)
		if (i+1)*sliceSize < numTransactions {
			minTxNumber = uint32(i * sliceSize)
			maxTxNumber = uint32((i+1)*sliceSize) - 1
		} else {
			minTxNumber = uint32(i * sliceSize)
			maxTxNumber = uint32(numTransactions) - 1
		}
		currentSlice := parsedBlock.Transactions[minTxNumber : maxTxNumber+1]
		resChannels[i] = p.startWorker(minTxNumber, maxTxNumber, currentSlice, blockNumber)
	}

	flattenedResults := merge(resChannels)
	depositChecks := []*messageStructures.DepositIndexCheckoutRequest{}
	withdrawChecks := []*messageStructures.WithdrawChallengeRequest{}
	for result := range flattenedResults {
		if result.Result == false {
			log.Println("Unsuccesfull result")
		}
		if result.NonrecoverableError != nil {
			log.Fatalln(result.NonrecoverableError)
		}
		if result.DepositIndexRequest != nil {
			depositChecks = append(depositChecks, result.DepositIndexRequest)
		}
		if result.WithdrawChallengeRequest != nil {
			withdrawChecks = append(withdrawChecks, result.WithdrawChallengeRequest)
		}
	}
	return depositChecks, withdrawChecks, nil
}

// simple merger that waits until all channels are closed
func merge(inputs []<-chan []ResultPayload) <-chan ResultPayload {
	var wg sync.WaitGroup
	out := make(chan ResultPayload)
	output := func(c <-chan []ResultPayload) {
		for n := range c {
			for _, r := range n {
				out <- r
			}
		}
		wg.Done()
	}
	wg.Add(len(inputs))
	for _, c := range inputs {
		go output(c)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

func (p *BlockProcessor) startWorker(startIndex, endIndex uint32, txes []*transaction.SignedTransaction, blockNumber uint32) <-chan []ResultPayload {
	finished := make(chan []ResultPayload)
	go func() {
		defer close(finished)
		preprocessed, err := p.PreprocessTransactions(startIndex, endIndex, txes, blockNumber)
		if err != nil {
			log.Fatalln(err)
		}
		results, err := p.CheckTransactionsSlice(preprocessed)
		if err != nil {
			if err != nil {
				log.Fatalln("Inconsistent DB state")
			}
		}
		results, err = p.ApplyTransactionsSlice(preprocessed, results)
		if err != nil {
			if err != nil {
				log.Fatalln("Inconsistent DB state")
			}
		}
		if err != nil {
			results, err := p.ProcessSliceInSequence(preprocessed)
			if err != nil {
				log.Fatalln(err)
			}
			finished <- results
		} else {
			finished <- results
		}
	}()
	return finished
}

// Preprocessing step does the following:
// 1. check for deposit transactions that there is a deposit record in database
// 2. parse transaction signature
// 3. make an array of UTXO indexes that should be marked as spent and write transaction index
// 4. make an array of fresh UTXOs
func (p *BlockProcessor) PreprocessTransactions(startIndex, endIndex uint32, txes []*transaction.SignedTransaction, blockNumber uint32) ([]*PreprocessedTransactionPayload, error) {
	numTxes := endIndex - startIndex + 1
	preprocessedPayloads := make([]*PreprocessedTransactionPayload, numTxes)
	// keysToDelete := make([][][]byte, numTxes)
	// keysToAdd := make([][][]byte, numTxes)
	// spendingIndexesToWrite := make([][][2][]byte, numTxes)
	for i := 0; i < int(numTxes); i++ {
		tx := txes[i]
		transactionNumber := startIndex + uint32(i)
		transactionType := tx.UnsignedTransaction.TransactionType
		payload := &PreprocessedTransactionPayload{transactionNumber, nil, nil, nil, nil}
		// iterate over inputs, add records that should be removed from UTXO set and added to spending index
		if transactionType[0] == transaction.TransactionTypeFund {
			request, err := p.MakeCheckDepositRecord(tx)
			if err != nil {
				return nil, err
			}
			payload.depositCheckoutRequest = request
		} else {
			numInputs := len(tx.UnsignedTransaction.Inputs)
			forDelete := make([][]byte, numInputs)
			forIndex := make([][2][]byte, numInputs)
			for j := range tx.UnsignedTransaction.Inputs {
				inputIndex, err := transaction.CreateCorrespondingUTXOIndexForInput(tx, j)
				if err != nil {
					return nil, err
				}
				prefixedInputIndex := append(UtxoIndexPrefix, inputIndex[:]...)
				spendingHistoryIndex, err := transaction.CreateShortUTXOIndexForInput(tx, j)
				if err != nil {
					return nil, err
				}
				spendingHistoryKey := append(SpendingIndexKey, spendingHistoryIndex...)
				spendingHistoryValue := transaction.PackUTXOnumber(blockNumber, transactionNumber, uint8(j))
				forDelete[j] = prefixedInputIndex
				spendingIndex := [2][]byte{}
				spendingIndex[0] = spendingHistoryKey
				spendingIndex[1] = spendingHistoryValue
				forIndex[j] = spendingIndex
			}
			payload.keysToDelete = forDelete
			payload.spendingIndexesToWrite = forIndex
		}

		numOutputs := len(tx.UnsignedTransaction.Outputs)
		forAdd := make([][]byte, numOutputs)
		// now we should iterate over the outputs to create new UTXOs in an index
		for j := range tx.UnsignedTransaction.Outputs {
			newUTXOindex, err := transaction.CreateUTXOIndexForOutput(tx, blockNumber, transactionNumber, j)
			if err != nil {
				return nil, err
			}
			fullUTXOindex := append(UtxoIndexPrefix, newUTXOindex[:]...)
			forAdd[j] = fullUTXOindex
		}
		payload.keysToWrite = forAdd
		preprocessedPayloads[i] = payload
	}
	return preprocessedPayloads, nil
}

// during the block processing parse database for deposits during parsing
func (p *BlockProcessor) MakeCheckDepositRecord(tx *transaction.SignedTransaction) (*messageStructures.DepositIndexCheckoutRequest, error) {
	depositIndexBytes := tx.UnsignedTransaction.Inputs[0].Value[:]
	depositIndex := big.NewInt(0).SetBytes(depositIndexBytes)
	depositFor := tx.UnsignedTransaction.Outputs[0].GetToAddress()
	depositAmount := big.NewInt(0).SetBytes(tx.UnsignedTransaction.Outputs[0].Value[:])
	sender, err := tx.GetFrom()
	if err != nil {
		return nil, err
	}
	depositForCast := common.Address{}
	copy(depositForCast[:], depositFor[:])
	operatorCast := common.Address{}
	copy(operatorCast[:], sender[:])
	request := &messageStructures.DepositIndexCheckoutRequest{depositIndex, depositAmount, depositForCast, operatorCast}
	return request, nil
}

func (p *BlockProcessor) ProcessSliceInSequence(preprocessed []*PreprocessedTransactionPayload) ([]ResultPayload, error) {
	results := make([]ResultPayload, len(preprocessed))
	return results, nil
}

func (p *BlockProcessor) CheckTransactionsSlice(preprocessed []*PreprocessedTransactionPayload) ([]ResultPayload, error) {
	results := make([]ResultPayload, len(preprocessed))
	txn := p.db.NewTransaction(false)
	defer txn.Discard()
	// first do an UTXO check
	for i, payload := range preprocessed {
		// process either a deposit transaction or work with UTXO indexes
		if payload.depositCheckoutRequest != nil {
			results[i] = ResultPayload{payload.txNumber, false, nil, payload.depositCheckoutRequest, nil}
		} else {
			for j, toDelete := range payload.keysToDelete {
				_, err := txn.Get(toDelete)
				if err != nil {
					// There is no UTXO allowed for spending, so it's either withdrawn or spent
					// with invalid signature or some other information
					spendingIndex := payload.spendingIndexesToWrite[j][1]
					withdrawRequest := &messageStructures.WithdrawChallengeRequest{PartialHash: nil, UtxoIndex: toDelete, SpendingTransactionIndex: spendingIndex}
					results[i] = ResultPayload{payload.txNumber, true, nil, nil, withdrawRequest}
					continue
				}
			}
		}
	}
	return results, nil
}

func (p *BlockProcessor) ApplyTransactionsSlice(preprocessed []*PreprocessedTransactionPayload, results []ResultPayload) ([]ResultPayload, error) {
	txn := p.db.NewTransaction(true)
	defer txn.Discard()
	// first do an UTXO check
	for i, payload := range preprocessed {
		if results[i].Result == true {
			continue
		}
		for _, toDelete := range payload.keysToDelete {
			err := txn.Delete(toDelete)
			if err != nil {
				return nil, err
			}
		}

		for _, toIndex := range payload.spendingIndexesToWrite {
			err := txn.Set(toIndex[0], toIndex[1])
			if err != nil {
				return nil, err
			}
		}

		// process new UTXOs
		for _, toAdd := range payload.keysToWrite {
			err := txn.Set(toAdd, []byte{0x01})
			if err != nil {
				return nil, err
			}
		}
		if results[i].DepositIndexRequest != nil {
			results[i].Result = true
		} else {
			res := ResultPayload{payload.txNumber, true, nil, nil, nil}
			results[i] = res
		}
	}
	err := txn.Commit(nil)
	if err != nil {
		return nil, err
	}
	return results, nil
}

// func (p *BlockProcessor) ProcessTransactionsSlice(preprocessed []*PreprocessedTransactionPayload) ([]ResultPayload, error) {
// 	results := make([]ResultPayload, len(preprocessed))
// 	txn := p.db.NewTransaction(true)
// 	defer txn.Discard()
// 	// first do an UTXO check
// 	for i, payload := range preprocessed {
// 		// process either a deposit transaction or work with UTXO indexes
// 		if payload.depositCheckoutRequest != nil {
// 			results[i] = ResultPayload{payload.txNumber, false, nil, payload.depositCheckoutRequest, nil}
// 		} else {
// 			for j, toDelete := range payload.keysToDelete {
// 				_, err := txn.Get(toDelete)
// 				if err == badger.ErrTxnTooBig {
// 					err := txn.Commit(nil)
// 					if err != nil {
// 						return nil, err
// 					}
// 					txn = p.db.NewTransaction(true)
// 					_, err = txn.Get(toDelete)
// 					if err != nil {
// 						spendingIndex := payload.spendingIndexesToWrite[j][1]
// 						withdrawRequest := &messageStructures.WithdrawChallengeRequest{toDelete, spendingIndex}
// 						results[i] = ResultPayload{payload.txNumber, true, nil, nil, withdrawRequest}
// 						continue
// 					}
// 				} else if err != nil {
// 					spendingIndex := payload.spendingIndexesToWrite[j][1]
// 					withdrawRequest := &messageStructures.WithdrawChallengeRequest{toDelete, spendingIndex}
// 					results[i] = ResultPayload{payload.txNumber, true, nil, nil, withdrawRequest}
// 					continue
// 				}
// 				err = txn.Delete(toDelete)
// 				if err == badger.ErrTxnTooBig {
// 					err := txn.Commit(nil)
// 					if err != nil {
// 						return nil, err
// 					}
// 					txn = p.db.NewTransaction(true)
// 					err = txn.Delete(toDelete)
// 					if err != nil {
// 						return nil, err
// 					}
// 				} else if err != nil {
// 					return nil, err
// 				}
// 			}

// 			for _, toIndex := range payload.spendingIndexesToWrite {
// 				if results[i].Result == true {
// 					continue
// 				}
// 				err := txn.Set(toIndex[0], toIndex[1])
// 				if err == badger.ErrTxnTooBig {
// 					err := txn.Commit(nil)
// 					if err != nil {
// 						return nil, err
// 					}
// 					txn = p.db.NewTransaction(true)
// 					err = txn.Set(toIndex[0], toIndex[1])
// 					if err != nil {
// 						return nil, err
// 					}
// 				} else if err != nil {
// 					return nil, err
// 				}
// 			}
// 		}

// 		// process new UTXOs
// 		for _, toAdd := range payload.keysToWrite {
// 			if results[i].Result == true {
// 				continue
// 			}
// 			err := txn.Set(toAdd, []byte{0x01})
// 			if err == badger.ErrTxnTooBig {
// 				err := txn.Commit(nil)
// 				if err != nil {
// 					return nil, err
// 				}
// 				txn = p.db.NewTransaction(true)
// 				err = txn.Set(toAdd, []byte{0x01})
// 				if err != nil {
// 					return nil, err
// 				}
// 			} else if err != nil {
// 				return nil, err
// 			}
// 		}

// 		if results[i].Result != true {
// 			if results[i].DepositIndexRequest != nil {
// 				results[i].Result = true
// 			} else {
// 				res := ResultPayload{payload.txNumber, true, nil, nil, nil}
// 				results[i] = res
// 			}

// 		}
// 	}
// 	err := txn.Commit(nil)
// 	if err != nil {
// 		return nil, err
// 	}
// 	return results, nil
// }

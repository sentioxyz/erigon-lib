/*
   Copyright 2022 Erigon contributors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package state

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/bits"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/holiman/uint256"
	"github.com/ledgerwatch/log/v3"

	"github.com/ledgerwatch/erigon-lib/kv/iter"
	"github.com/ledgerwatch/erigon-lib/kv/order"

	"github.com/ledgerwatch/erigon-lib/commitment"
	"github.com/ledgerwatch/erigon-lib/common/length"
	"github.com/ledgerwatch/erigon-lib/kv"
)

// StepsInBiggestFile - files of this size are completely frozen/immutable.
// files of smaller size are also immutable, but can be removed after merge to bigger files.
const StepsInBiggestFile = 32

var (
	mxTxProcessed       = metrics.GetOrCreateCounter("domain_tx_processed")
	mxRunningMerges     = metrics.GetOrCreateCounter("domain_running_merges")
	mxCollatingProgress = metrics.GetOrCreateCounter("domain_collating_progress")
	mxPruningProgress   = metrics.GetOrCreateCounter("domain_pruning_progress")
	mxCollateTimes      = metrics.GetOrCreateSummary("domain_collate_times")
	mxBuildTimes        = metrics.GetOrCreateSummary("domain_build_files_times")
	mxPruningTimes      = metrics.GetOrCreateSummary("domain_pruning_times")
)

type Aggregator struct {
	aggregationStep uint64
	accounts        *Domain
	storage         *Domain
	code            *Domain
	commitment      *DomainCommitted
	logAddrs        *InvertedIndex
	logTopics       *InvertedIndex
	tracesFrom      *InvertedIndex
	tracesTo        *InvertedIndex
	txNum           uint64
	seekTxNum       uint64
	blockNum        uint64
	stepDoneNotice  chan [length.Hash]byte
	rwTx            kv.RwTx
	stats           FilesStats
	tmpdir          string
	defaultCtx      *AggregatorContext
}

func NewAggregator(dir, tmpdir string, aggregationStep uint64, commitmentMode CommitmentMode, commitTrieVariant commitment.TrieVariant) (*Aggregator, error) {
	a := &Aggregator{aggregationStep: aggregationStep, tmpdir: tmpdir, stepDoneNotice: make(chan [length.Hash]byte, 1)}

	closeAgg := true
	defer func() {
		if closeAgg {
			a.Close()
		}
	}()
	err := os.MkdirAll(dir, 0764)
	if err != nil {
		return nil, err
	}
	if a.accounts, err = NewDomain(dir, tmpdir, aggregationStep, "accounts", kv.AccountKeys, kv.AccountVals, kv.AccountHistoryKeys, kv.AccountHistoryVals, kv.AccountSettings, kv.AccountIdx, false /* compressVals */, false); err != nil {
		return nil, err
	}
	if a.storage, err = NewDomain(dir, tmpdir, aggregationStep, "storage", kv.StorageKeys, kv.StorageVals, kv.StorageHistoryKeys, kv.StorageHistoryVals, kv.StorageSettings, kv.StorageIdx, false /* compressVals */, false); err != nil {
		return nil, err
	}
	if a.code, err = NewDomain(dir, tmpdir, aggregationStep, "code", kv.CodeKeys, kv.CodeVals, kv.CodeHistoryKeys, kv.CodeHistoryVals, kv.CodeSettings, kv.CodeIdx, true /* compressVals */, true); err != nil {
		return nil, err
	}

	commitd, err := NewDomain(dir, tmpdir, aggregationStep, "commitment", kv.CommitmentKeys, kv.CommitmentVals, kv.CommitmentHistoryKeys, kv.CommitmentHistoryVals, kv.CommitmentSettings, kv.CommitmentIdx, false /* compressVals */, true)
	if err != nil {
		return nil, err
	}
	a.commitment = NewCommittedDomain(commitd, commitmentMode, commitTrieVariant)

	if a.logAddrs, err = NewInvertedIndex(dir, tmpdir, aggregationStep, "logaddrs", kv.LogAddressKeys, kv.LogAddressIdx, false, nil); err != nil {
		return nil, err
	}
	if a.logTopics, err = NewInvertedIndex(dir, tmpdir, aggregationStep, "logtopics", kv.LogTopicsKeys, kv.LogTopicsIdx, false, nil); err != nil {
		return nil, err
	}
	if a.tracesFrom, err = NewInvertedIndex(dir, tmpdir, aggregationStep, "tracesfrom", kv.TracesFromKeys, kv.TracesFromIdx, false, nil); err != nil {
		return nil, err
	}
	if a.tracesTo, err = NewInvertedIndex(dir, tmpdir, aggregationStep, "tracesto", kv.TracesToKeys, kv.TracesToIdx, false, nil); err != nil {
		return nil, err
	}
	closeAgg = false

	a.seekTxNum = a.EndTxNumMinimax()
	return a, nil
}

func (a *Aggregator) ReopenFolder() error {
	var err error
	if err = a.accounts.OpenFolder(); err != nil {
		return fmt.Errorf("OpenFolder: %w", err)
	}
	if err = a.storage.OpenFolder(); err != nil {
		return fmt.Errorf("OpenFolder: %w", err)
	}
	if err = a.code.OpenFolder(); err != nil {
		return fmt.Errorf("OpenFolder: %w", err)
	}
	if err = a.commitment.OpenFolder(); err != nil {
		return fmt.Errorf("OpenFolder: %w", err)
	}
	if err = a.logAddrs.OpenFolder(); err != nil {
		return fmt.Errorf("OpenFolder: %w", err)
	}
	if err = a.logTopics.OpenFolder(); err != nil {
		return fmt.Errorf("OpenFolder: %w", err)
	}
	if err = a.tracesFrom.OpenFolder(); err != nil {
		return fmt.Errorf("OpenFolder: %w", err)
	}
	if err = a.tracesTo.OpenFolder(); err != nil {
		return fmt.Errorf("OpenFolder: %w", err)
	}
	return nil
}

func (a *Aggregator) ReopenList(fNames []string) error {
	var err error
	if err = a.accounts.OpenList(fNames); err != nil {
		return err
	}
	if err = a.storage.OpenList(fNames); err != nil {
		return err
	}
	if err = a.code.OpenList(fNames); err != nil {
		return err
	}
	if err = a.commitment.OpenList(fNames); err != nil {
		return err
	}
	if err = a.logAddrs.OpenList(fNames); err != nil {
		return err
	}
	if err = a.logTopics.OpenList(fNames); err != nil {
		return err
	}
	if err = a.tracesFrom.OpenList(fNames); err != nil {
		return err
	}
	if err = a.tracesTo.OpenList(fNames); err != nil {
		return err
	}
	return nil
}

func (a *Aggregator) GetAndResetStats() DomainStats {
	stats := DomainStats{}
	stats.Accumulate(a.accounts.GetAndResetStats())
	stats.Accumulate(a.storage.GetAndResetStats())
	stats.Accumulate(a.code.GetAndResetStats())
	stats.Accumulate(a.commitment.GetAndResetStats())

	var tto, tfrom, ltopics, laddr DomainStats
	tto.FilesCount, tto.DataSize, tto.IndexSize = a.tracesTo.collectFilesStat()
	tfrom.FilesCount, tfrom.DataSize, tfrom.DataSize = a.tracesFrom.collectFilesStat()
	ltopics.FilesCount, ltopics.DataSize, ltopics.IndexSize = a.logTopics.collectFilesStat()
	laddr.FilesCount, laddr.DataSize, laddr.IndexSize = a.logAddrs.collectFilesStat()

	stats.Accumulate(tto)
	stats.Accumulate(tfrom)
	stats.Accumulate(ltopics)
	stats.Accumulate(laddr)
	return stats
}

func (a *Aggregator) Close() {
	if a.defaultCtx != nil {
		a.defaultCtx.Close()
	}
	if a.stepDoneNotice != nil {
		close(a.stepDoneNotice)
	}
	if a.accounts != nil {
		a.accounts.Close()
	}
	if a.storage != nil {
		a.storage.Close()
	}
	if a.code != nil {
		a.code.Close()
	}
	if a.commitment != nil {
		a.commitment.Close()
	}

	if a.logAddrs != nil {
		a.logAddrs.Close()
	}
	if a.logTopics != nil {
		a.logTopics.Close()
	}
	if a.tracesFrom != nil {
		a.tracesFrom.Close()
	}
	if a.tracesTo != nil {
		a.tracesTo.Close()
	}
}

func (a *Aggregator) SetTx(tx kv.RwTx) {
	a.rwTx = tx
	a.accounts.SetTx(tx)
	a.storage.SetTx(tx)
	a.code.SetTx(tx)
	a.commitment.SetTx(tx)
	a.logAddrs.SetTx(tx)
	a.logTopics.SetTx(tx)
	a.tracesFrom.SetTx(tx)
	a.tracesTo.SetTx(tx)
}

func (a *Aggregator) SetTxNum(txNum uint64) {
	a.txNum = txNum
	a.accounts.SetTxNum(txNum)
	a.storage.SetTxNum(txNum)
	a.code.SetTxNum(txNum)
	a.commitment.SetTxNum(txNum)
	a.logAddrs.SetTxNum(txNum)
	a.logTopics.SetTxNum(txNum)
	a.tracesFrom.SetTxNum(txNum)
	a.tracesTo.SetTxNum(txNum)
}

func (a *Aggregator) SetWorkers(i int) {
	a.accounts.compressWorkers = i
	a.storage.compressWorkers = i
	a.code.compressWorkers = i
	a.commitment.compressWorkers = i
	a.logAddrs.compressWorkers = i
	a.logTopics.compressWorkers = i
	a.tracesFrom.compressWorkers = i
	a.tracesTo.compressWorkers = i
}

func (a *Aggregator) SetCommitmentMode(mode CommitmentMode) {
	a.commitment.mode = mode
}

func (a *Aggregator) EndTxNumMinimax() uint64 {
	min := a.accounts.endTxNumMinimax()
	if txNum := a.storage.endTxNumMinimax(); txNum < min {
		min = txNum
	}
	if txNum := a.code.endTxNumMinimax(); txNum < min {
		min = txNum
	}
	if txNum := a.commitment.endTxNumMinimax(); txNum < min {
		min = txNum
	}
	if txNum := a.logAddrs.endTxNumMinimax(); txNum < min {
		min = txNum
	}
	if txNum := a.logTopics.endTxNumMinimax(); txNum < min {
		min = txNum
	}
	if txNum := a.tracesFrom.endTxNumMinimax(); txNum < min {
		min = txNum
	}
	if txNum := a.tracesTo.endTxNumMinimax(); txNum < min {
		min = txNum
	}
	return min
}

func (a *Aggregator) SeekCommitment() (txNum uint64, err error) {
	filesTxNum := a.EndTxNumMinimax()
	txNum, err = a.commitment.SeekCommitment(a.aggregationStep, filesTxNum)
	if err != nil {
		return 0, err
	}
	if txNum == 0 {
		return
	}
	a.seekTxNum = txNum + 1
	return txNum + 1, nil
}

func (a *Aggregator) aggregate(ctx context.Context, step uint64) error {
	var (
		logEvery = time.NewTicker(time.Second * 30)
		wg       sync.WaitGroup
		errCh    = make(chan error, 8)
		maxSpan  = StepsInBiggestFile * a.aggregationStep
		txFrom   = step * a.aggregationStep
		txTo     = (step + 1) * a.aggregationStep
		workers  = 1

		stepStartedAt = time.Now()
	)

	defer logEvery.Stop()

	for i, d := range []*Domain{a.accounts, a.storage, a.code, a.commitment.Domain} {
		wg.Add(1)

		mxCollatingProgress.Inc()
		start := time.Now()
		collation, err := d.collateStream(ctx, step, txFrom, txTo, d.tx, logEvery)
		mxCollateTimes.UpdateDuration(start)
		mxCollatingProgress.Dec()

		if err != nil {
			collation.Close()
			return fmt.Errorf("domain collation %q has failed: %w", d.filenameBase, err)
		}

		go func(wg *sync.WaitGroup, d *Domain, collation Collation) {
			defer wg.Done()
			mxRunningMerges.Inc()

			start := time.Now()
			sf, err := d.buildFiles(ctx, step, collation)
			collation.Close()

			if err != nil {
				errCh <- err

				sf.Close()
				mxRunningMerges.Dec()
				return
			}

			mxRunningMerges.Dec()

			d.integrateFiles(sf, step*a.aggregationStep, (step+1)*a.aggregationStep)
			d.stats.LastFileBuildingTook = time.Since(start)
		}(&wg, d, collation)

		if i != 3 { // do not warmup commitment domain
			if err := d.warmup(ctx, txFrom, d.aggregationStep/10, d.tx); err != nil {
				return fmt.Errorf("warmup %q domain failed: %w", d.filenameBase, err)
			}
		}
		mxPruningProgress.Inc()
		start = time.Now()
		if err := d.prune(ctx, step, txFrom, txTo, math.MaxUint64, logEvery); err != nil {
			return err
		}
		mxPruningTimes.UpdateDuration(start)
		mxPruningProgress.Dec()
	}

	// indices are built concurrently
	for _, d := range []*InvertedIndex{a.logTopics, a.logAddrs, a.tracesFrom, a.tracesTo} {
		wg.Add(1)

		mxCollatingProgress.Inc()
		collation, err := d.collate(ctx, step*a.aggregationStep, (step+1)*a.aggregationStep, d.tx, logEvery)
		mxCollatingProgress.Dec()

		if err != nil {
			return fmt.Errorf("index collation %q has failed: %w", d.filenameBase, err)
		}

		go func(wg *sync.WaitGroup, d *InvertedIndex, tx kv.Tx) {
			defer wg.Done()

			mxRunningMerges.Inc()
			start := time.Now()

			sf, err := d.buildFiles(ctx, step, collation)
			if err != nil {
				errCh <- err
				sf.Close()
				return
			}

			mxRunningMerges.Dec()
			mxBuildTimes.UpdateDuration(start)

			d.integrateFiles(sf, step*a.aggregationStep, (step+1)*a.aggregationStep)

			icx := d.MakeContext()
			mxRunningMerges.Inc()

			if err := d.mergeRangesUpTo(ctx, d.endTxNumMinimax(), maxSpan, workers, icx); err != nil {
				errCh <- err

				mxRunningMerges.Dec()
				icx.Close()
				return
			}

			mxRunningMerges.Dec()
			icx.Close()
		}(&wg, d, d.tx)

		mxPruningProgress.Inc()
		startPrune := time.Now()
		if err := d.prune(ctx, txFrom, txTo, math.MaxUint64, logEvery); err != nil {
			return err
		}
		mxPruningTimes.UpdateDuration(startPrune)
		mxPruningProgress.Dec()
	}

	go func() {
		wg.Wait()
		close(errCh)
	}()

	for err := range errCh {
		log.Warn("domain collate-buildFiles failed", "err", err)
		return fmt.Errorf("domain collate-build failed: %w", err)
	}

	var clo, chi, plo, phi, blo, bhi time.Duration
	clo, plo, blo = time.Hour*99, time.Hour*99, time.Hour*99
	for _, s := range []DomainStats{a.accounts.stats, a.code.stats, a.storage.stats} {
		c := s.LastCollationTook
		p := s.LastPruneTook
		b := s.LastFileBuildingTook

		if c < clo {
			clo = c
		}
		if c > chi {
			chi = c
		}
		if p < plo {
			plo = p
		}
		if p > phi {
			phi = p
		}
		if b < blo {
			blo = b
		}
		if b > bhi {
			bhi = b
		}
	}

	stepTook := time.Since(stepStartedAt)
	log.Info("[stat] finished aggregation, ready for mergeUpTo",
		"range", fmt.Sprintf("%.2fM-%.2fM", float64(txFrom)/10e5, float64(txTo)/10e5),
		"step_took", stepTook,
		"collate_min", clo, "collate_max", chi,
		"prune_min", plo, "prune_max", phi,
		"files_build_min", blo, "files_build_max", bhi)

	mergeStartedAt := time.Now()
	maxEndTxNum := a.EndTxNumMinimax()

	var upmerges int
	for {
		a.defaultCtx.Close()
		a.defaultCtx = a.MakeContext()

		mxRunningMerges.Inc()
		somethingMerged, err := a.mergeLoopStep(ctx, maxEndTxNum, 1)
		if err != nil {
			mxRunningMerges.Dec()
			return err
		}
		mxRunningMerges.Dec()

		if !somethingMerged {
			break
		}
		upmerges++
	}

	log.Info("[stat] aggregation merged",
		"upto_tx", maxEndTxNum,
		"aggregation_took", time.Since(stepStartedAt),
		"step_took", stepTook,
		"merge_took", time.Since(mergeStartedAt),
		"merges_count", upmerges)
	return nil
}

func (a *Aggregator) mergeLoopStep(ctx context.Context, maxEndTxNum uint64, workers int) (somethingDone bool, err error) {
	closeAll := true
	mergeStartedAt := time.Now()

	maxSpan := a.aggregationStep * StepsInBiggestFile
	r := a.findMergeRange(maxEndTxNum, maxSpan)
	if !r.any() {
		return false, nil
	}

	outs := a.staticFilesInRange(r, a.defaultCtx)
	defer func() {
		if closeAll {
			outs.Close()
		}
	}()

	in, err := a.mergeFiles(ctx, outs, r, workers)
	if err != nil {
		return true, err
	}
	defer func() {
		if closeAll {
			in.Close()
		}
	}()
	a.integrateMergedFiles(outs, in)
	a.cleanAfterFreeze(in)
	closeAll = false

	var blo, bhi time.Duration
	blo = time.Hour * 99
	for _, s := range []DomainStats{a.accounts.stats, a.code.stats, a.storage.stats} {
		b := s.LastFileBuildingTook
		if b < blo {
			blo = b
		}
		if b > bhi {
			bhi = b
		}
	}

	log.Info("[stat] finished merge step",
		"upto_tx", maxEndTxNum, "merge_step_took", time.Since(mergeStartedAt),
		"merge_min", blo, "merge_max", bhi)

	return true, nil
}

type Ranges struct {
	accounts   DomainRanges
	storage    DomainRanges
	code       DomainRanges
	commitment DomainRanges
}

func (r Ranges) String() string {
	return fmt.Sprintf("accounts=%s, storage=%s, code=%s, commitment=%s", r.accounts.String(), r.storage.String(), r.code.String(), r.commitment.String())
}

func (r Ranges) any() bool {
	return r.accounts.any() || r.storage.any() || r.code.any() || r.commitment.any()
}

func (a *Aggregator) findMergeRange(maxEndTxNum, maxSpan uint64) Ranges {
	var r Ranges
	r.accounts = a.accounts.findMergeRange(maxEndTxNum, maxSpan)
	r.storage = a.storage.findMergeRange(maxEndTxNum, maxSpan)
	r.code = a.code.findMergeRange(maxEndTxNum, maxSpan)
	r.commitment = a.commitment.findMergeRange(maxEndTxNum, maxSpan)
	log.Info(fmt.Sprintf("findMergeRange(%d, %d)=%+v\n", maxEndTxNum, maxSpan, r))
	return r
}

type SelectedStaticFiles struct {
	accounts       []*filesItem
	accountsIdx    []*filesItem
	accountsHist   []*filesItem
	storage        []*filesItem
	storageIdx     []*filesItem
	storageHist    []*filesItem
	code           []*filesItem
	codeIdx        []*filesItem
	codeHist       []*filesItem
	commitment     []*filesItem
	commitmentIdx  []*filesItem
	commitmentHist []*filesItem
	codeI          int
	storageI       int
	accountsI      int
	commitmentI    int
}

func (sf SelectedStaticFiles) Close() {
	for _, group := range [][]*filesItem{
		sf.accounts, sf.accountsIdx, sf.accountsHist,
		sf.storage, sf.storageIdx, sf.storageHist,
		sf.code, sf.codeIdx, sf.codeHist,
		sf.commitment, sf.commitmentIdx, sf.commitmentHist,
	} {
		for _, item := range group {
			if item != nil {
				if item.decompressor != nil {
					item.decompressor.Close()
				}
				if item.index != nil {
					item.index.Close()
				}
				if item.bindex != nil {
					item.bindex.Close()
				}
			}
		}
	}
}

func (a *Aggregator) staticFilesInRange(r Ranges, ac *AggregatorContext) SelectedStaticFiles {
	var sf SelectedStaticFiles
	if r.accounts.any() {
		sf.accounts, sf.accountsIdx, sf.accountsHist, sf.accountsI = a.accounts.staticFilesInRange(r.accounts, ac.accounts)
	}
	if r.storage.any() {
		sf.storage, sf.storageIdx, sf.storageHist, sf.storageI = a.storage.staticFilesInRange(r.storage, ac.storage)
	}
	if r.code.any() {
		sf.code, sf.codeIdx, sf.codeHist, sf.codeI = a.code.staticFilesInRange(r.code, ac.code)
	}
	if r.commitment.any() {
		sf.commitment, sf.commitmentIdx, sf.commitmentHist, sf.commitmentI = a.commitment.staticFilesInRange(r.commitment, ac.commitment)
	}
	return sf
}

type MergedFiles struct {
	accounts                      *filesItem
	accountsIdx, accountsHist     *filesItem
	storage                       *filesItem
	storageIdx, storageHist       *filesItem
	code                          *filesItem
	codeIdx, codeHist             *filesItem
	commitment                    *filesItem
	commitmentIdx, commitmentHist *filesItem
}

func (mf MergedFiles) Close() {
	for _, item := range []*filesItem{
		mf.accounts, mf.accountsIdx, mf.accountsHist,
		mf.storage, mf.storageIdx, mf.storageHist,
		mf.code, mf.codeIdx, mf.codeHist,
		mf.commitment, mf.commitmentIdx, mf.commitmentHist,
		//mf.logAddrs, mf.logTopics, mf.tracesFrom, mf.tracesTo,
	} {
		if item != nil {
			if item.decompressor != nil {
				item.decompressor.Close()
			}
			if item.decompressor != nil {
				item.index.Close()
			}
			if item.bindex != nil {
				item.bindex.Close()
			}
		}
	}
}

func (a *Aggregator) mergeFiles(ctx context.Context, files SelectedStaticFiles, r Ranges, workers int) (MergedFiles, error) {
	started := time.Now()
	defer func(t time.Time) {
		log.Info("[snapshots] domain files has been merged",
			"range", fmt.Sprintf("%d-%d", r.accounts.valuesStartTxNum/a.aggregationStep, r.accounts.valuesEndTxNum/a.aggregationStep),
			"took", time.Since(t))
	}(started)

	var mf MergedFiles
	closeFiles := true
	defer func() {
		if closeFiles {
			mf.Close()
		}
	}()

	var (
		errCh      = make(chan error, 4)
		wg         sync.WaitGroup
		predicates sync.WaitGroup
	)

	wg.Add(4)
	predicates.Add(2)

	go func(predicates *sync.WaitGroup) {
		defer wg.Done()
		defer predicates.Done()
		var err error
		if r.accounts.any() {
			if mf.accounts, mf.accountsIdx, mf.accountsHist, err = a.accounts.mergeFiles(ctx, files.accounts, files.accountsIdx, files.accountsHist, r.accounts, workers); err != nil {
				errCh <- err
			}
		}
	}(&predicates)
	go func(predicates *sync.WaitGroup) {
		defer wg.Done()
		defer predicates.Done()
		var err error
		if r.storage.any() {
			if mf.storage, mf.storageIdx, mf.storageHist, err = a.storage.mergeFiles(ctx, files.storage, files.storageIdx, files.storageHist, r.storage, workers); err != nil {
				errCh <- err
			}
		}
	}(&predicates)
	go func() {
		defer wg.Done()

		var err error
		if r.code.any() {
			if mf.code, mf.codeIdx, mf.codeHist, err = a.code.mergeFiles(ctx, files.code, files.codeIdx, files.codeHist, r.code, workers); err != nil {
				errCh <- err
			}
		}
	}()

	go func(preidcates *sync.WaitGroup) {
		defer wg.Done()
		predicates.Wait()

		var err error
		// requires storage|accounts to be merged at this point
		if r.commitment.any() {
			if mf.commitment, mf.commitmentIdx, mf.commitmentHist, err = a.commitment.mergeFiles(ctx, files, mf, r.commitment, workers); err != nil {
				errCh <- err
			}
		}

	}(&predicates)

	go func() {
		wg.Wait()
		close(errCh)
	}()

	var lastError error
	for err := range errCh {
		lastError = err
	}
	if lastError == nil {
		closeFiles = false
	}
	return mf, lastError
}

func (a *Aggregator) integrateMergedFiles(outs SelectedStaticFiles, in MergedFiles) {
	a.accounts.integrateMergedFiles(outs.accounts, outs.accountsIdx, outs.accountsHist, in.accounts, in.accountsIdx, in.accountsHist)
	a.storage.integrateMergedFiles(outs.storage, outs.storageIdx, outs.storageHist, in.storage, in.storageIdx, in.storageHist)
	a.code.integrateMergedFiles(outs.code, outs.codeIdx, outs.codeHist, in.code, in.codeIdx, in.codeHist)
	a.commitment.integrateMergedFiles(outs.commitment, outs.commitmentIdx, outs.commitmentHist, in.commitment, in.commitmentIdx, in.commitmentHist)
}

func (a *Aggregator) cleanAfterFreeze(in MergedFiles) {
	a.accounts.cleanAfterFreeze(in.accountsHist)
	a.storage.cleanAfterFreeze(in.storageHist)
	a.code.cleanAfterFreeze(in.codeHist)
	a.commitment.cleanAfterFreeze(in.commitment)
}

// ComputeCommitment evaluates commitment for processed state.
// If `saveStateAfter`=true, then trie state will be saved to DB after commitment evaluation.
func (a *Aggregator) ComputeCommitment(saveStateAfter, trace bool) (rootHash []byte, err error) {
	// if commitment mode is Disabled, there will be nothing to compute on.
	rootHash, branchNodeUpdates, err := a.commitment.ComputeCommitment(trace)
	if err != nil {
		return nil, err
	}
	if a.seekTxNum > a.txNum {
		saveStateAfter = false
	}

	for pref, update := range branchNodeUpdates {
		prefix := []byte(pref)

		stateValue, err := a.defaultCtx.ReadCommitment(prefix, a.rwTx)
		if err != nil {
			return nil, err
		}

		stated := commitment.BranchData(stateValue)
		merged, err := a.commitment.branchMerger.Merge(stated, update)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(stated, merged) {
			continue
		}
		if trace {
			fmt.Printf("computeCommitment merge [%x] [%x]+[%x]=>[%x]\n", prefix, stated, update, merged)
		}
		if err = a.UpdateCommitmentData(prefix, merged); err != nil {
			return nil, err
		}
	}

	if saveStateAfter {
		if err := a.commitment.storeCommitmentState(a.blockNum, a.txNum); err != nil {
			return nil, err
		}
	}

	return rootHash, nil
}

// Provides channel which receives commitment hash each time aggregation is occured
func (a *Aggregator) AggregatedRoots() chan [length.Hash]byte {
	return a.stepDoneNotice
}

func (a *Aggregator) notifyAggregated(rootHash []byte) {
	rh := (*[length.Hash]byte)(rootHash[:])
	select {
	case a.stepDoneNotice <- *rh:
	default:
	}
}

func (a *Aggregator) ReadyToFinishTx() bool {
	return (a.txNum+1)%a.aggregationStep == 0 && a.seekTxNum < a.txNum
}

func (a *Aggregator) FinishTx() (err error) {
	atomic.AddUint64(&a.stats.TxCount, 1)
	mxTxProcessed.Inc()

	if !a.ReadyToFinishTx() {
		return nil
	}

	mxRunningMerges.Inc()
	defer mxRunningMerges.Dec()

	a.commitment.patriciaTrie.ResetFns(a.defaultCtx.branchFn, a.defaultCtx.accountFn, a.defaultCtx.storageFn)
	rootHash, err := a.ComputeCommitment(true, false)
	if err != nil {
		return err
	}
	step := a.txNum / a.aggregationStep
	if step == 0 {
		a.notifyAggregated(rootHash)
		return nil
	}
	step-- // Leave one step worth in the DB
	if err := a.Flush(context.TODO()); err != nil {
		return err
	}

	ctx := context.Background()
	if err := a.aggregate(ctx, step); err != nil {
		return err
	}

	a.notifyAggregated(rootHash)
	return nil
}

func (a *Aggregator) UpdateAccountData(addr []byte, account []byte) error {
	a.commitment.TouchPlainKey(addr, account, a.commitment.TouchPlainKeyAccount)
	return a.accounts.Put(addr, nil, account)
}

func (a *Aggregator) UpdateAccountCode(addr []byte, code []byte) error {
	a.commitment.TouchPlainKey(addr, code, a.commitment.TouchPlainKeyCode)
	if len(code) == 0 {
		return a.code.Delete(addr, nil)
	}
	return a.code.Put(addr, nil, code)
}

func (a *Aggregator) UpdateCommitmentData(prefix []byte, code []byte) error {
	return a.commitment.Put(prefix, nil, code)
}

func (a *Aggregator) DeleteAccount(addr []byte) error {
	a.commitment.TouchPlainKey(addr, nil, a.commitment.TouchPlainKeyAccount)

	if err := a.accounts.Delete(addr, nil); err != nil {
		return err
	}
	if err := a.code.Delete(addr, nil); err != nil {
		return err
	}
	var e error
	if err := a.storage.defaultDc.IteratePrefix(addr, func(k, _ []byte) {
		a.commitment.TouchPlainKey(k, nil, a.commitment.TouchPlainKeyStorage)
		if e == nil {
			e = a.storage.Delete(k, nil)
		}
	}); err != nil {
		return err
	}
	return e
}

func (a *Aggregator) WriteAccountStorage(addr, loc []byte, value []byte) error {
	composite := make([]byte, len(addr)+len(loc))
	copy(composite, addr)
	copy(composite[length.Addr:], loc)

	a.commitment.TouchPlainKey(composite, value, a.commitment.TouchPlainKeyStorage)
	if len(value) == 0 {
		return a.storage.Delete(addr, loc)
	}
	return a.storage.Put(addr, loc, value)
}

func (a *Aggregator) AddTraceFrom(addr []byte) error {
	return a.tracesFrom.Add(addr)
}

func (a *Aggregator) AddTraceTo(addr []byte) error {
	return a.tracesTo.Add(addr)
}

func (a *Aggregator) AddLogAddr(addr []byte) error {
	return a.logAddrs.Add(addr)
}

func (a *Aggregator) AddLogTopic(topic []byte) error {
	return a.logTopics.Add(topic)
}

// StartWrites - pattern: `defer agg.StartWrites().FinishWrites()`
func (a *Aggregator) StartWrites() *Aggregator {
	a.accounts.StartWrites()
	a.storage.StartWrites()
	a.code.StartWrites()
	a.commitment.StartWrites()
	a.logAddrs.StartWrites()
	a.logTopics.StartWrites()
	a.tracesFrom.StartWrites()
	a.tracesTo.StartWrites()

	if a.defaultCtx != nil {
		a.defaultCtx.Close()
	}
	a.defaultCtx = &AggregatorContext{
		a:          a,
		accounts:   a.accounts.defaultDc,
		storage:    a.storage.defaultDc,
		code:       a.code.defaultDc,
		commitment: a.commitment.defaultDc,
		logAddrs:   a.logAddrs.MakeContext(),
		logTopics:  a.logTopics.MakeContext(),
		tracesFrom: a.tracesFrom.MakeContext(),
		tracesTo:   a.tracesTo.MakeContext(),
	}
	a.commitment.patriciaTrie.ResetFns(a.defaultCtx.branchFn, a.defaultCtx.accountFn, a.defaultCtx.storageFn)
	return a
}

func (a *Aggregator) FinishWrites() {
	a.accounts.FinishWrites()
	a.storage.FinishWrites()
	a.code.FinishWrites()
	a.commitment.FinishWrites()
	a.logAddrs.FinishWrites()
	a.logTopics.FinishWrites()
	a.tracesFrom.FinishWrites()
	a.tracesTo.FinishWrites()
}

// Flush - must be called before Collate, if you did some writes
func (a *Aggregator) Flush(ctx context.Context) error {
	flushers := []flusher{
		a.accounts.Rotate(),
		a.storage.Rotate(),
		a.code.Rotate(),
		a.commitment.Domain.Rotate(),
		a.logAddrs.Rotate(),
		a.logTopics.Rotate(),
		a.tracesFrom.Rotate(),
		a.tracesTo.Rotate(),
	}
	defer func(t time.Time) { log.Debug("[snapshots] history flush", "took", time.Since(t)) }(time.Now())
	for _, f := range flushers {
		if err := f.Flush(ctx, a.rwTx); err != nil {
			return err
		}
	}
	return nil
}

type FilesStats struct {
	HistoryReads uint64
	TotalReads   uint64
	IdxAccess    time.Duration
	TxCount      uint64
	FilesCount   uint64
	IdxSize      uint64
	DataSize     uint64
}

func (a *Aggregator) Stats() FilesStats {
	res := a.stats
	stat := a.GetAndResetStats()
	res.IdxSize = stat.IndexSize
	res.DataSize = stat.DataSize
	res.FilesCount = stat.FilesCount
	res.HistoryReads = stat.HistoryQueries.Load()
	res.TotalReads = stat.TotalQueries.Load()
	res.IdxAccess = stat.EfSearchTime
	return res
}

type AggregatorContext struct {
	a          *Aggregator
	accounts   *DomainContext
	storage    *DomainContext
	code       *DomainContext
	commitment *DomainContext
	logAddrs   *InvertedIndexContext
	logTopics  *InvertedIndexContext
	tracesFrom *InvertedIndexContext
	tracesTo   *InvertedIndexContext
	keyBuf     []byte
}

func (a *Aggregator) MakeContext() *AggregatorContext {
	return &AggregatorContext{
		a:          a,
		accounts:   a.accounts.MakeContext(),
		storage:    a.storage.MakeContext(),
		code:       a.code.MakeContext(),
		commitment: a.commitment.MakeContext(),
		logAddrs:   a.logAddrs.MakeContext(),
		logTopics:  a.logTopics.MakeContext(),
		tracesFrom: a.tracesFrom.MakeContext(),
		tracesTo:   a.tracesTo.MakeContext(),
	}
}

func (ac *AggregatorContext) ReadAccountData(addr []byte, roTx kv.Tx) ([]byte, error) {
	return ac.accounts.Get(addr, nil, roTx)
}

func (ac *AggregatorContext) ReadAccountDataBeforeTxNum(addr []byte, txNum uint64, roTx kv.Tx) ([]byte, error) {
	v, err := ac.accounts.GetBeforeTxNum(addr, txNum, roTx)
	return v, err
}

func (ac *AggregatorContext) ReadAccountStorage(addr []byte, loc []byte, roTx kv.Tx) ([]byte, error) {
	return ac.storage.Get(addr, loc, roTx)
}

func (ac *AggregatorContext) ReadAccountStorageBeforeTxNum(addr []byte, loc []byte, txNum uint64, roTx kv.Tx) ([]byte, error) {
	if cap(ac.keyBuf) < len(addr)+len(loc) {
		ac.keyBuf = make([]byte, len(addr)+len(loc))
	} else if len(ac.keyBuf) != len(addr)+len(loc) {
		ac.keyBuf = ac.keyBuf[:len(addr)+len(loc)]
	}
	copy(ac.keyBuf, addr)
	copy(ac.keyBuf[len(addr):], loc)
	v, err := ac.storage.GetBeforeTxNum(ac.keyBuf, txNum, roTx)
	return v, err
}

func (ac *AggregatorContext) ReadAccountCode(addr []byte, roTx kv.Tx) ([]byte, error) {
	return ac.code.Get(addr, nil, roTx)
}

func (ac *AggregatorContext) ReadCommitment(addr []byte, roTx kv.Tx) ([]byte, error) {
	return ac.commitment.Get(addr, nil, roTx)
}

func (ac *AggregatorContext) ReadCommitmentBeforeTxNum(addr []byte, txNum uint64, roTx kv.Tx) ([]byte, error) {
	v, err := ac.commitment.GetBeforeTxNum(addr, txNum, roTx)
	return v, err
}

func (ac *AggregatorContext) ReadAccountCodeBeforeTxNum(addr []byte, txNum uint64, roTx kv.Tx) ([]byte, error) {
	v, err := ac.code.GetBeforeTxNum(addr, txNum, roTx)
	return v, err
}

func (ac *AggregatorContext) ReadAccountCodeSize(addr []byte, roTx kv.Tx) (int, error) {
	code, err := ac.code.Get(addr, nil, roTx)
	if err != nil {
		return 0, err
	}
	return len(code), nil
}

func (ac *AggregatorContext) ReadAccountCodeSizeBeforeTxNum(addr []byte, txNum uint64, roTx kv.Tx) (int, error) {
	code, err := ac.code.GetBeforeTxNum(addr, txNum, roTx)
	if err != nil {
		return 0, err
	}
	return len(code), nil
}

func (a *AggregatorContext) branchFn(prefix []byte) ([]byte, error) {
	// Look in the summary table first
	stateValue, err := a.ReadCommitment(prefix, a.a.rwTx)
	if err != nil {
		return nil, fmt.Errorf("failed read branch %x: %w", commitment.CompactedKeyToHex(prefix), err)
	}
	if stateValue == nil {
		return nil, nil
	}
	// fmt.Printf("Returning branch data prefix [%x], mergeVal=[%x]\n", commitment.CompactedKeyToHex(prefix), stateValue)
	return stateValue[2:], nil // Skip touchMap but keep afterMap
}

func (a *AggregatorContext) accountFn(plainKey []byte, cell *commitment.Cell) error {
	encAccount, err := a.ReadAccountData(plainKey, a.a.rwTx)
	if err != nil {
		return err
	}
	cell.Nonce = 0
	cell.Balance.Clear()
	copy(cell.CodeHash[:], commitment.EmptyCodeHash)
	if len(encAccount) > 0 {
		nonce, balance, chash := DecodeAccountBytes(encAccount)
		cell.Nonce = nonce
		cell.Balance.Set(balance)
		if chash != nil {
			copy(cell.CodeHash[:], chash)
		}
	}

	code, err := a.ReadAccountCode(plainKey, a.a.rwTx)
	if err != nil {
		return err
	}
	if code != nil {
		a.a.commitment.keccak.Reset()
		a.a.commitment.keccak.Write(code)
		copy(cell.CodeHash[:], a.a.commitment.keccak.Sum(nil))
	}
	cell.Delete = len(encAccount) == 0 && len(code) == 0
	return nil
}

func (a *AggregatorContext) storageFn(plainKey []byte, cell *commitment.Cell) error {
	// Look in the summary table first
	enc, err := a.ReadAccountStorage(plainKey[:length.Addr], plainKey[length.Addr:], a.a.rwTx)
	if err != nil {
		return err
	}
	cell.StorageLen = len(enc)
	copy(cell.Storage[:], enc)
	cell.Delete = cell.StorageLen == 0
	return nil
}

func (ac *AggregatorContext) LogAddrIterator(addr []byte, startTxNum, endTxNum int, roTx kv.Tx) (iter.U64, error) {
	return ac.logAddrs.IterateRange(addr, startTxNum, endTxNum, order.Asc, -1, roTx)
}

func (ac *AggregatorContext) LogTopicIterator(topic []byte, startTxNum, endTxNum int, roTx kv.Tx) (iter.U64, error) {
	return ac.logTopics.IterateRange(topic, startTxNum, endTxNum, order.Asc, -1, roTx)
}

func (ac *AggregatorContext) TraceFromIterator(addr []byte, startTxNum, endTxNum int, roTx kv.Tx) (iter.U64, error) {
	return ac.tracesFrom.IterateRange(addr, startTxNum, endTxNum, order.Asc, -1, roTx)
}

func (ac *AggregatorContext) TraceToIterator(addr []byte, startTxNum, endTxNum int, roTx kv.Tx) (iter.U64, error) {
	return ac.tracesTo.IterateRange(addr, startTxNum, endTxNum, order.Asc, -1, roTx)
}

func (ac *AggregatorContext) Close() {
	ac.accounts.Close()
	ac.storage.Close()
	ac.code.Close()
	ac.commitment.Close()
	ac.logAddrs.Close()
	ac.logTopics.Close()
	ac.tracesFrom.Close()
	ac.tracesTo.Close()
}

func DecodeAccountBytes(enc []byte) (nonce uint64, balance *uint256.Int, hash []byte) {
	balance = new(uint256.Int)

	if len(enc) > 0 {
		pos := 0
		nonceBytes := int(enc[pos])
		pos++
		if nonceBytes > 0 {
			nonce = bytesToUint64(enc[pos : pos+nonceBytes])
			pos += nonceBytes
		}
		balanceBytes := int(enc[pos])
		pos++
		if balanceBytes > 0 {
			balance.SetBytes(enc[pos : pos+balanceBytes])
			pos += balanceBytes
		}
		codeHashBytes := int(enc[pos])
		pos++
		if codeHashBytes > 0 {
			codeHash := make([]byte, length.Hash)
			copy(codeHash, enc[pos:pos+codeHashBytes])
		}
	}
	return
}

func EncodeAccountBytes(nonce uint64, balance *uint256.Int, hash []byte, incarnation uint64) []byte {
	l := int(1)
	if nonce > 0 {
		l += (bits.Len64(nonce) + 7) / 8
	}
	l++
	if !balance.IsZero() {
		l += balance.ByteLen()
	}
	l++
	if len(hash) == length.Hash {
		l += 32
	}
	l++
	if incarnation > 0 {
		l += (bits.Len64(incarnation) + 7) / 8
	}
	value := make([]byte, l)
	pos := 0

	if nonce == 0 {
		value[pos] = 0
		pos++
	} else {
		nonceBytes := (bits.Len64(nonce) + 7) / 8
		value[pos] = byte(nonceBytes)
		var nonce = nonce
		for i := nonceBytes; i > 0; i-- {
			value[pos+i] = byte(nonce)
			nonce >>= 8
		}
		pos += nonceBytes + 1
	}
	if balance.IsZero() {
		value[pos] = 0
		pos++
	} else {
		balanceBytes := balance.ByteLen()
		value[pos] = byte(balanceBytes)
		pos++
		balance.WriteToSlice(value[pos : pos+balanceBytes])
		pos += balanceBytes
	}
	if len(hash) == 0 {
		value[pos] = 0
		pos++
	} else {
		value[pos] = 32
		pos++
		copy(value[pos:pos+32], hash[:])
		pos += 32
	}
	if incarnation == 0 {
		value[pos] = 0
	} else {
		incBytes := (bits.Len64(incarnation) + 7) / 8
		value[pos] = byte(incBytes)
		var inc = incarnation
		for i := incBytes; i > 0; i-- {
			value[pos+i] = byte(inc)
			inc >>= 8
		}
	}
	return value
}

func bytesToUint64(buf []byte) (x uint64) {
	for i, b := range buf {
		x = x<<8 + uint64(b)
		if i == 7 {
			return
		}
	}
	return
}

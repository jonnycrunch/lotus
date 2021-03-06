package storage

import (
	"context"
	"time"

	ffi "github.com/filecoin-project/filecoin-ffi"
	sectorbuilder "github.com/filecoin-project/go-sectorbuilder"
	"go.opencensus.io/trace"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/actors"
	"github.com/filecoin-project/lotus/chain/types"
)

func (s *FPoStScheduler) failPost(eps uint64) {
	s.failLk.Lock()
	if eps > s.failed {
		s.failed = eps
	}
	s.failLk.Unlock()
}

func (s *FPoStScheduler) doPost(ctx context.Context, eps uint64, ts *types.TipSet) {
	ctx, abort := context.WithCancel(ctx)

	s.abort = abort
	s.activeEPS = eps

	go func() {
		defer abort()

		ctx, span := trace.StartSpan(ctx, "FPoStScheduler.doPost")
		defer span.End()

		proof, err := s.runPost(ctx, eps, ts)
		if err != nil {
			log.Errorf("runPost failed: %+v", err)
			s.failPost(eps)
			return
		}

		if err := s.submitPost(ctx, proof); err != nil {
			log.Errorf("submitPost failed: %+v", err)
			s.failPost(eps)
			return
		}

	}()
}

func (s *FPoStScheduler) declareFaults(ctx context.Context, fc uint64, params *actors.DeclareFaultsParams) error {
	log.Warnf("DECLARING %d FAULTS", fc)

	enc, aerr := actors.SerializeParams(params)
	if aerr != nil {
		return xerrors.Errorf("could not serialize declare faults parameters: %w", aerr)
	}

	msg := &types.Message{
		To:       s.actor,
		From:     s.worker,
		Method:   actors.MAMethods.DeclareFaults,
		Params:   enc,
		Value:    types.NewInt(0),
		GasLimit: types.NewInt(10000000), // i dont know help
		GasPrice: types.NewInt(1),
	}

	sm, err := s.api.MpoolPushMessage(ctx, msg)
	if err != nil {
		return xerrors.Errorf("pushing faults message to mpool: %w", err)
	}

	rec, err := s.api.StateWaitMsg(ctx, sm.Cid())
	if err != nil {
		return xerrors.Errorf("waiting for declare faults: %w", err)
	}

	if rec.Receipt.ExitCode != 0 {
		return xerrors.Errorf("declare faults exit %d", rec.Receipt.ExitCode)
	}

	log.Infof("Faults declared successfully")
	return nil
}

func (s *FPoStScheduler) checkFaults(ctx context.Context, ssi sectorbuilder.SortedPublicSectorInfo) ([]uint64, error) {
	faults := s.sb.Scrub(ssi)

	declaredFaults := map[uint64]struct{}{}

	{
		chainFaults, err := s.api.StateMinerFaults(ctx, s.actor, nil)
		if err != nil {
			return nil, xerrors.Errorf("checking on-chain faults: %w", err)
		}

		for _, fault := range chainFaults {
			declaredFaults[fault] = struct{}{}
		}
	}

	if len(faults) > 0 {
		params := &actors.DeclareFaultsParams{Faults: types.NewBitField()}

		for _, fault := range faults {
			if _, ok := declaredFaults[fault.SectorID]; ok {
				continue
			}

			log.Warnf("new fault detected: sector %d: %s", fault.SectorID, fault.Err)
			declaredFaults[fault.SectorID] = struct{}{}
			params.Faults.Set(fault.SectorID)
		}

		pc, err := params.Faults.Count()
		if err != nil {
			return nil, xerrors.Errorf("counting faults: %w", err)
		}
		if pc > 0 {
			if err := s.declareFaults(ctx, pc, params); err != nil {
				return nil, err
			}
		}
	}

	faultIDs := make([]uint64, 0, len(declaredFaults))
	for fault := range declaredFaults {
		faultIDs = append(faultIDs, fault)
	}

	return faultIDs, nil
}

func (s *FPoStScheduler) runPost(ctx context.Context, eps uint64, ts *types.TipSet) (*actors.SubmitFallbackPoStParams, error) {
	ctx, span := trace.StartSpan(ctx, "storage.runPost")
	defer span.End()

	challengeRound := int64(eps + build.FallbackPoStDelay)

	rand, err := s.api.ChainGetRandomness(ctx, ts.Key(), challengeRound)
	if err != nil {
		return nil, xerrors.Errorf("failed to get chain randomness for fpost (ts=%d; eps=%d): %w", ts.Height(), eps, err)
	}

	ssi, err := s.sortedSectorInfo(ctx, ts)
	if err != nil {
		return nil, xerrors.Errorf("getting sorted sector info: %w", err)
	}

	log.Infow("running fPoSt",
		"chain-random", rand,
		"eps", eps,
		"height", ts.Height())

	faults, err := s.checkFaults(ctx, ssi)
	if err != nil {
		log.Errorf("Failed to declare faults: %+v", err)
	}

	tsStart := time.Now()

	var seed [32]byte
	copy(seed[:], rand)

	log.Infow("generating fPoSt",
		"sectors", len(ssi.Values()),
		"faults", len(faults))

	scandidates, proof, err := s.sb.GenerateFallbackPoSt(ssi, seed, faults)
	if err != nil {
		return nil, xerrors.Errorf("running post failed: %w", err)
	}

	elapsed := time.Since(tsStart)
	log.Infow("submitting PoSt", "pLen", len(proof), "elapsed", elapsed)

	candidates := make([]types.EPostTicket, len(scandidates))
	for i, sc := range scandidates {
		part := make([]byte, 32)
		copy(part, sc.PartialTicket[:])
		candidates[i] = types.EPostTicket{
			Partial:        part,
			SectorID:       sc.SectorID,
			ChallengeIndex: sc.SectorChallengeIndex,
		}
	}

	return &actors.SubmitFallbackPoStParams{
		Proof:      proof,
		Candidates: candidates,
	}, nil
}

func (s *FPoStScheduler) sortedSectorInfo(ctx context.Context, ts *types.TipSet) (sectorbuilder.SortedPublicSectorInfo, error) {
	sset, err := s.api.StateMinerProvingSet(ctx, s.actor, ts)
	if err != nil {
		return sectorbuilder.SortedPublicSectorInfo{}, xerrors.Errorf("failed to get proving set for miner (tsH: %d): %w", ts.Height(), err)
	}
	if len(sset) == 0 {
		log.Warn("empty proving set! (ts.H: %d)", ts.Height())
	}

	sbsi := make([]ffi.PublicSectorInfo, len(sset))
	for k, sector := range sset {
		var commR [sectorbuilder.CommLen]byte
		copy(commR[:], sector.CommR)

		sbsi[k] = ffi.PublicSectorInfo{
			SectorID: sector.SectorID,
			CommR:    commR,
		}
	}

	return sectorbuilder.NewSortedPublicSectorInfo(sbsi), nil
}

func (s *FPoStScheduler) submitPost(ctx context.Context, proof *actors.SubmitFallbackPoStParams) error {
	ctx, span := trace.StartSpan(ctx, "storage.commitPost")
	defer span.End()

	enc, aerr := actors.SerializeParams(proof)
	if aerr != nil {
		return xerrors.Errorf("could not serialize submit post parameters: %w", aerr)
	}

	msg := &types.Message{
		To:       s.actor,
		From:     s.worker,
		Method:   actors.MAMethods.SubmitFallbackPoSt,
		Params:   enc,
		Value:    types.NewInt(1000),     // currently hard-coded late fee in actor, returned if not late
		GasLimit: types.NewInt(10000000), // i dont know help
		GasPrice: types.NewInt(1),
	}

	// TODO: consider maybe caring about the output
	sm, err := s.api.MpoolPushMessage(ctx, msg)
	if err != nil {
		return xerrors.Errorf("pushing message to mpool: %w", err)
	}

	log.Infof("Submitted fallback post: %s", sm.Cid())

	go func() {
		rec, err := s.api.StateWaitMsg(context.TODO(), sm.Cid())
		if err != nil {
			log.Error(err)
			return
		}

		if rec.Receipt.ExitCode == 0 {
			return
		}

		log.Errorf("Submitting fallback post %s failed: exit %d", sm.Cid(), rec.Receipt.ExitCode)
	}()

	return nil
}

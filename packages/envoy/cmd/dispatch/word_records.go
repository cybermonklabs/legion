package main

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/modelpace"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/wordrecords"
)

// newWordRecords builds the records job of INFRA-3231's R8 (wordrecords.Job), or nil where it is
// off: DISPATCH_WORD_RECORDS=off, or its model group (DISPATCH_WORD_RECORDS_MODEL_*) unset, each of
// which it logs. The job reads the event log up to horizon's safe horizon from
// DISPATCH_WORD_RECORDS_FROM, and its calls are charged to its group's share of its deployment on
// pacer. It is built from settings resolveWritingSettings has already checked, so an error here is
// a shape that check missed; it calls nothing and reads no file.
func newWordRecords(cfg writingSettings, pacer *modelpace.Pacer, database *store.Store, horizon *events.Horizon) (*wordrecords.Job, error) {
	switch {
	case !cfg.WordRecords:
		slog.Info("dispatch: the records of the words people ask about are off", "setting", "DISPATCH_WORD_RECORDS")
		return nil, nil
	case cfg.WordRecordsModel.Kind == "":
		slog.Info("dispatch: the records of the words people ask about are off: the records job's model group is unset", "setting", wordRecordsModelSpec.prefix+"KIND")
		return nil, nil
	}
	model, err := cfg.WordRecordsModel.agentloopModel()
	if err != nil {
		return nil, fmt.Errorf("the records job's model: %w", err)
	}
	pace, err := cfg.WordRecordsModel.pace(pacer)
	if err != nil {
		return nil, fmt.Errorf("the records job's model: %w", err)
	}
	slog.Info("dispatch: records the words people ask agents about", "kind", cfg.WordRecordsModel.Kind,
		"deployment", cfg.WordRecordsModel.Deployment, "stand_in", cfg.WordRecordsModel.StandIn,
		"from", cfg.WordRecordsFrom.Format(time.RFC3339))
	return wordrecords.New(wordrecords.Config{
		Store:      database,
		Horizon:    horizon,
		Classifier: wordrecords.Classifier{Model: model, Pace: pace},
		From:       cfg.WordRecordsFrom,
	}), nil
}

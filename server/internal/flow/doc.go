// Package flow drives Blurt's capture and agent workers together — both stages,
// a real database, real streams or in-memory ones — so cross-component behaviour
// is tested as one pipeline rather than per unit.
//
// A failing test should be traceable without reading the whole package, so the
// files are split by what a reader is looking for:
//
//	harness_test.go     the pump: the two workers, the two streams, start/stop, claimers
//	doubles_test.go     every test double: model, pusher, transcriber, classifier, tagger, gateway
//	helpers_test.go     what a test calls on the pump: create, reply, settle, recall
//	flows_test.go       the journeys: capture → clarification → reply → records
//	recovery_test.go    things going wrong: claimer, sweeper, status guard
//	timeout_test.go     slow providers, and the state left behind afterwards
//	redis_flow_test.go  the same pipeline over real Redis (opt-in, skips by default)
//
// This is a package of tests only: the pump deliberately lives beside the tests
// it serves rather than in a non-test file nothing else imports.
package flow

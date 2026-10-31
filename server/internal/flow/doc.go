// Package flow drives Blurt's capture and agent workers together — both stages, a real database,
// real streams or in-memory ones — so cross-component behaviour is tested as one pipeline rather
// than per unit.
//
// The package is tests only: the pump lives beside the tests it serves rather than in a non-test
// file nothing else imports.
package flow

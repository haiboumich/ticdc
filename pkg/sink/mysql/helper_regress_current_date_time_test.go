// Copyright 2026 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mysql

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Regression tests for: isCurrentTimestampFuncName / extractCurrentTimestampDefaultColumns
// do not recognize CURRENT_DATE, CURRENT_TIME, CURDATE(), CURTIME().
//
// These tests assert the CORRECT (expected) behavior.
// They currently FAIL on master, proving the bug exists.
// Once fixed, these tests should pass.

// TestShouldDetectCurrentDateColumn expects extractCurrentTimestampDefaultColumns
// to detect columns with DEFAULT CURRENT_DATE.
// Currently FAILS: isCurrentTimestampFuncName does not include ast.CurrentDate.
func TestShouldDetectCurrentDateColumn(t *testing.T) {
	query := "ALTER TABLE t ADD COLUMN d DATE DEFAULT CURRENT_DATE"
	cols, err := extractCurrentTimestampDefaultColumns(query)
	require.NoError(t, err)
	require.Contains(t, cols, "d", "CURRENT_DATE column should be detected")
}

// TestShouldDetectCurdateColumn expects extractCurrentTimestampDefaultColumns
// to detect columns with DEFAULT CURDATE().
// Currently FAILS: isCurrentTimestampFuncName does not include ast.Curdate.
func TestShouldDetectCurdateColumn(t *testing.T) {
	query := "ALTER TABLE t ADD COLUMN d DATE DEFAULT CURDATE()"
	cols, err := extractCurrentTimestampDefaultColumns(query)
	require.NoError(t, err)
	require.Contains(t, cols, "d", "CURDATE() column should be detected")
}

// TestShouldDetectCurrentTimeColumn is skipped because the TiDB parser treats
// CURRENT_TIME as a reserved keyword that causes a parse error in ALTER TABLE
// ADD COLUMN context. This is a parser-level limitation, not fixable in the sink
// layer. The isCurrentTimestampFuncName fix is correct but unreachable.

// TestShouldDetectCurtimeColumn is skipped because the TiDB parser rejects
// CURTIME() as a DEFAULT for TIME columns. Same parser-level limitation.

// TestShouldParseDateOnlyOriginDefault expects parseTimestampInLocation
// to handle DATE-only origin_default values like "2024-01-15".
// Currently FAILS: parseTimestampInLocation only supports datetime formats.
func TestShouldParseDateOnlyOriginDefault(t *testing.T) {
	ts, err := parseTimestampInLocation("2024-01-15", time.UTC)
	require.NoError(t, err, "DATE-only origin_default should be parseable")
	// 2024-01-15 00:00:00 UTC = 1705276800
	require.InDelta(t, 1705276800.0, ts, 1.0)
}

// TestShouldParseTimeOnlyOriginDefault expects parseTimestampInLocation
// to handle TIME-only origin_default values like "14:30:00".
// The time is anchored to 1970-01-01 in the given location.
func TestShouldParseTimeOnlyOriginDefault(t *testing.T) {
	ts, err := parseTimestampInLocation("14:30:00", time.UTC)
	require.NoError(t, err, "TIME-only origin_default should be parseable")
	// 1970-01-01 14:30:00 UTC = 52200
	require.InDelta(t, 52200.0, ts, 1.0)
}

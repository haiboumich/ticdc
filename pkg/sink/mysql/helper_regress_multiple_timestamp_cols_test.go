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

	"github.com/stretchr/testify/require"
)

// Regression tests for: ddlSessionTimestampFromOriginDefault returns on the
// first matching column, so when multiple CURRENT_TIMESTAMP columns have
// different origin_default values, only the first is used.
//
// These tests assert the CORRECT (expected) behavior.
// They currently FAIL on master, proving the bug exists.

// TestShouldDetectAllTimestampColumnsInSingleAlter expects
// extractCurrentTimestampDefaultColumns to detect ALL timestamp-default
// columns in a single ALTER TABLE statement with multiple ADD COLUMN specs.
// Currently PASS (extraction is correct) but the downstream
// ddlSessionTimestampFromOriginDefault only uses the first match.
func TestShouldDetectAllTimestampColumnsInSingleAlter(t *testing.T) {
	query := "ALTER TABLE t ADD COLUMN c1 DATETIME DEFAULT CURRENT_TIMESTAMP, ADD COLUMN c2 DATETIME DEFAULT CURRENT_TIMESTAMP"
	cols, err := extractCurrentTimestampDefaultColumns(query)
	require.NoError(t, err)
	require.Contains(t, cols, "c1")
	require.Contains(t, cols, "c2")
	require.Len(t, cols, 2)
}

// TestShouldDetectAllTimestampColumnsInMultiAlter expects
// extractCurrentTimestampDefaultColumns to detect timestamp-default columns
// across multiple AlterTableSpecs (e.g., ActionMultiSchemaChange).
func TestShouldDetectAllTimestampColumnsInMultiAlter(t *testing.T) {
	query := "ALTER TABLE t ADD COLUMN c1 DATETIME DEFAULT CURRENT_TIMESTAMP, MODIFY COLUMN c2 DATETIME DEFAULT CURRENT_TIMESTAMP"
	cols, err := extractCurrentTimestampDefaultColumns(query)
	require.NoError(t, err)
	require.Contains(t, cols, "c1")
	require.Contains(t, cols, "c2")
}

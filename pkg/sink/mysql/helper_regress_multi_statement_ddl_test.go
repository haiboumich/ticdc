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

// Regression tests for: extractCurrentTimestampDefaultColumns cannot handle
// multi-statement DDL (e.g. ActionCreateTables with multiple CREATE TABLE).
//
// These tests assert the CORRECT (expected) behavior.
// They currently FAIL on master because ParseOneStmt cannot parse multi-statement DDLs.
// Once fixed, these tests should pass.

// TestShouldDetectTimestampInMultiStatementDDL_SecondTable expects
// extractCurrentTimestampDefaultColumns to find CURRENT_TIMESTAMP columns
// in the second CREATE TABLE of a multi-statement DDL.
// Currently FAILS: ParseOneStmt returns syntax error for multi-statement input.
func TestShouldDetectTimestampInMultiStatementDDL_SecondTable(t *testing.T) {
	query := "CREATE TABLE t1 (id INT PRIMARY KEY); CREATE TABLE t2 (id INT PRIMARY KEY, ts DATETIME DEFAULT CURRENT_TIMESTAMP)"
	cols, err := extractCurrentTimestampDefaultColumns(query)
	require.NoError(t, err, "Multi-statement DDL should be parseable")
	require.Contains(t, cols, "ts", "CURRENT_TIMESTAMP column in the second CREATE TABLE should be detected")
}

// TestShouldDetectTimestampInMultiStatementDDL_BothTables expects
// extractCurrentTimestampDefaultColumns to find CURRENT_TIMESTAMP columns
// in both CREATE TABLE statements of a multi-statement DDL.
// Currently FAILS: ParseOneStmt returns syntax error for multi-statement input.
func TestShouldDetectTimestampInMultiStatementDDL_BothTables(t *testing.T) {
	query := "CREATE TABLE t1 (id INT PRIMARY KEY, ts1 DATETIME DEFAULT CURRENT_TIMESTAMP); CREATE TABLE t2 (id INT PRIMARY KEY, ts2 DATETIME DEFAULT CURRENT_TIMESTAMP)"
	cols, err := extractCurrentTimestampDefaultColumns(query)
	require.NoError(t, err, "Multi-statement DDL should be parseable")
	require.Contains(t, cols, "ts1", "CURRENT_TIMESTAMP column in the first CREATE TABLE should be detected")
	require.Contains(t, cols, "ts2", "CURRENT_TIMESTAMP column in the second CREATE TABLE should be detected")
}

// TestShouldDetectTimestampInMultiAlterDDL expects
// extractCurrentTimestampDefaultColumns to handle multiple ALTER TABLE statements
// in a single DDL query.
// Currently FAILS: ParseOneStmt returns syntax error for multi-statement input.
func TestShouldDetectTimestampInMultiAlterDDL(t *testing.T) {
	query := "ALTER TABLE t1 ADD COLUMN ts1 DATETIME DEFAULT CURRENT_TIMESTAMP; ALTER TABLE t2 ADD COLUMN ts2 DATETIME DEFAULT CURRENT_TIMESTAMP"
	cols, err := extractCurrentTimestampDefaultColumns(query)
	require.NoError(t, err, "Multi-statement ALTER TABLE DDL should be parseable")
	require.Contains(t, cols, "ts1", "CURRENT_TIMESTAMP column in first ALTER TABLE should be detected")
	require.Contains(t, cols, "ts2", "CURRENT_TIMESTAMP column in second ALTER TABLE should be detected")
}

// TestShouldDetectTimestampInMultiCreateFirstTableOnly expects
// extractCurrentTimestampDefaultColumns to still find the column even when
// only the first CREATE TABLE has a CURRENT_TIMESTAMP default.
// Currently FAILS: ParseOneStmt returns syntax error for multi-statement input.
func TestShouldDetectTimestampInMultiCreateFirstTableOnly(t *testing.T) {
	query := "CREATE TABLE t1 (id INT PRIMARY KEY, ts DATETIME DEFAULT CURRENT_TIMESTAMP); CREATE TABLE t2 (id INT PRIMARY KEY)"
	cols, err := extractCurrentTimestampDefaultColumns(query)
	require.NoError(t, err, "Multi-statement DDL should be parseable")
	require.Contains(t, cols, "ts", "CURRENT_TIMESTAMP column in the first CREATE TABLE should be detected")
}

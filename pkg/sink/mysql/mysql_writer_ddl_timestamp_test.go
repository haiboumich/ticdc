// Copyright 2025 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package mysql

import (
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	commonEvent "github.com/pingcap/ticdc/pkg/common/event"
	timodel "github.com/pingcap/tidb/pkg/meta/model"
	"github.com/stretchr/testify/require"
)

func ddlSessionTimestampForTest(event *commonEvent.DDLEvent, timezone string) (string, bool) {
	if event == nil {
		return "", false
	}
	ts, ok := ddlSessionTimestampFromOriginDefault(event, timezone)
	if !ok {
		return "", false
	}
	return formatUnixTimestamp(ts), true
}

func expectDDLExec(mock sqlmock.Sqlmock, event *commonEvent.DDLEvent, timezone string) {
	ddlTimestamp, ok := ddlSessionTimestampForTest(event, timezone)
	mock.ExpectExec("SET TIMESTAMP = DEFAULT").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if ok {
		mock.ExpectExec("SET TIMESTAMP = " + ddlTimestamp).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectExec(event.GetDDLQuery()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if ok {
		mock.ExpectExec("SET TIMESTAMP = DEFAULT").
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
}

func TestExecDDL_UsesOriginDefaultTimestampForCurrentTimestampDefault(t *testing.T) {
	writer, db, mock := newTestMysqlWriter(t)
	defer db.Close()
	writer.cfg.Timezone = "\"UTC\""

	helper := commonEvent.NewEventTestHelperWithTimeZone(t, time.UTC)
	defer helper.Close()

	helper.Tk().MustExec("use test")
	helper.Tk().MustExec("set time_zone = 'UTC'")
	helper.Tk().MustExec("set @@timestamp = 1720000000.123456")
	helper.DDL2Event("create table t (id int primary key)")

	ddlEvent := helper.DDL2Event("alter table t add column updatetime datetime(6) default current_timestamp(6)")

	originTs, ok := ddlSessionTimestampFromOriginDefault(ddlEvent, writer.cfg.Timezone)
	require.True(t, ok)
	ddlTimestamp, ok := ddlSessionTimestampForTest(ddlEvent, writer.cfg.Timezone)
	require.True(t, ok)
	require.Equal(t, ddlTimestamp, formatUnixTimestamp(originTs))

	mock.ExpectBegin()
	mock.ExpectExec("USE `test`;").WillReturnResult(sqlmock.NewResult(1, 1))
	expectDDLExec(mock, ddlEvent, writer.cfg.Timezone)
	mock.ExpectCommit()

	err := writer.execDDL(ddlEvent)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestExecDDL_CurrentDateDefaultDoesNotAlignTimestamp(t *testing.T) {
	// This test demonstrates that CURRENT_DATE/CURDATE defaults are NOT handled
	// by ddlSessionTimestampFromOriginDefault. The function only recognizes
	// CURRENT_TIMESTAMP/NOW/LOCALTIME/LOCALTIMESTAMP functions.
	// If the upstream and downstream clocks differ, the DATE default value
	// will be inconsistent.

	writer, db, mock := newTestMysqlWriter(t)
	defer db.Close()
	writer.cfg.Timezone = "\"UTC\""

	helper := commonEvent.NewEventTestHelperWithTimeZone(t, time.UTC)
	defer helper.Close()

	helper.Tk().MustExec("use test")
	helper.Tk().MustExec("set time_zone = 'UTC'")
	helper.Tk().MustExec("set @@timestamp = 1720000000.123456")
	helper.DDL2Event("create table t (id int primary key)")

	// Add a column with CURRENT_DATE default
	ddlEvent := helper.DDL2Event("alter table t add column d date default current_date")

	// Verify the column has a non-empty OriginDefaultValue
	columns := ddlEvent.TableInfo.GetColumns()
	var dateCol *timodel.ColumnInfo
	for _, col := range columns {
		if col.Name.L == "d" {
			dateCol = col
			break
		}
	}
	require.NotNil(t, dateCol)
	originDefault := dateCol.GetOriginDefaultValue()
	require.NotNil(t, originDefault, "CURRENT_DATE column should have OriginDefaultValue")
	t.Logf("CURRENT_DATE column OriginDefaultValue: %v (type: %T)", originDefault, originDefault)

	// But ddlSessionTimestampFromOriginDefault does NOT detect CURRENT_DATE
	_, ok := ddlSessionTimestampFromOriginDefault(ddlEvent, writer.cfg.Timezone)
	require.False(t, ok,
		"ddlSessionTimestampFromOriginDefault should return false for CURRENT_DATE default, "+
			"meaning no SET TIMESTAMP will be issued and the downstream may get a different date")

	// Verify the execDDL mock expectations: no SET TIMESTAMP = <value> will be issued
	mock.ExpectBegin()
	mock.ExpectExec("USE `test`;").WillReturnResult(sqlmock.NewResult(1, 1))
	// Only reset before DDL, no SET TIMESTAMP = <specific_value>
	mock.ExpectExec("SET TIMESTAMP = DEFAULT").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(ddlEvent.GetDDLQuery()).WillReturnResult(sqlmock.NewResult(1, 1))
	// No reset after DDL because useSessionTimestamp is false
	mock.ExpectCommit()

	err := writer.execDDL(ddlEvent)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestExecDDL_CurDateDefaultDoesNotAlignTimestamp(t *testing.T) {
	writer, db, mock := newTestMysqlWriter(t)
	defer db.Close()
	writer.cfg.Timezone = "\"UTC\""

	helper := commonEvent.NewEventTestHelperWithTimeZone(t, time.UTC)
	defer helper.Close()

	helper.Tk().MustExec("use test")
	helper.Tk().MustExec("set time_zone = 'UTC'")
	helper.Tk().MustExec("set @@timestamp = 1720000000.123456")
	helper.DDL2Event("create table t (id int primary key)")

	ddlEvent := helper.DDL2Event("alter table t add column d date default curdate()")

	columns := ddlEvent.TableInfo.GetColumns()
	var dateCol *timodel.ColumnInfo
	for _, col := range columns {
		if col.Name.L == "d" {
			dateCol = col
			break
		}
	}
	require.NotNil(t, dateCol)
	originDefault := dateCol.GetOriginDefaultValue()
	require.NotNil(t, originDefault, "CURDATE column should have OriginDefaultValue")
	t.Logf("CURDATE column OriginDefaultValue: %v (type: %T)", originDefault, originDefault)

	_, ok := ddlSessionTimestampFromOriginDefault(ddlEvent, writer.cfg.Timezone)
	require.False(t, ok,
		"ddlSessionTimestampFromOriginDefault should return false for CURDATE default")

	mock.ExpectBegin()
	mock.ExpectExec("USE `test`;").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("SET TIMESTAMP = DEFAULT").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(ddlEvent.GetDDLQuery()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	err := writer.execDDL(ddlEvent)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestExtractCurrentTimestampDefaultColumns_ExcludesCurrentDate(t *testing.T) {
	// Verify that CURRENT_DATE / CURDATE are not recognized by
	// extractCurrentTimestampDefaultColumns. These time functions are excluded
	// from the timestamp alignment logic, which means DDLs with these defaults
	// will NOT have SET TIMESTAMP applied and may produce inconsistent values.

	tests := []struct {
		name  string
		query string
	}{
		{name: "CURRENT_DATE", query: "CREATE TABLE t (id INT, d DATE DEFAULT CURRENT_DATE)"},
		{name: "CURDATE", query: "CREATE TABLE t (id INT, d DATE DEFAULT CURDATE())"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cols, err := extractCurrentTimestampDefaultColumns(tc.query)
			require.NoError(t, err)
			require.Empty(t, cols,
				"%s should not be detected as a timestamp default function", tc.name)
		})
	}
}

func TestExtractCurrentTimestampDefaultColumns_CurrentTimeNotValidInTiDB(t *testing.T) {
	// CURRENT_TIME / CURTIME cannot be used as column default values in TiDB.
	// The parser rejects these DDLs outright, so the inconsistency cannot
	// occur in practice. This test documents the expected parse failure.

	tests := []struct {
		name  string
		query string
	}{
		{name: "CURRENT_TIME", query: "CREATE TABLE t (id INT, c DATETIME DEFAULT CURRENT_TIME)"},
		{name: "CURTIME", query: "CREATE TABLE t (id INT, c DATETIME DEFAULT CURTIME())"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := extractCurrentTimestampDefaultColumns(tc.query)
			require.Error(t, err,
				"%s should fail to parse in TiDB (not valid as column default)", tc.name)
		})
	}
}

func TestExtractCurrentTimestampDefaultColumns_IncludesTimestampFunctions(t *testing.T) {
	// Verify that CURRENT_TIMESTAMP / NOW / LOCALTIME / LOCALTIMESTAMP are recognized.
	tests := []struct {
		name       string
		query      string
		expectCol  string
	}{
		{name: "CURRENT_TIMESTAMP", query: "ALTER TABLE t ADD COLUMN c DATETIME DEFAULT CURRENT_TIMESTAMP", expectCol: "c"},
		{name: "CURRENT_TIMESTAMP(6)", query: "ALTER TABLE t ADD COLUMN c DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6)", expectCol: "c"},
		{name: "NOW", query: "ALTER TABLE t ADD COLUMN c DATETIME DEFAULT NOW()", expectCol: "c"},
		{name: "LOCALTIME", query: "ALTER TABLE t ADD COLUMN c DATETIME DEFAULT LOCALTIME", expectCol: "c"},
		{name: "LOCALTIMESTAMP", query: "ALTER TABLE t ADD COLUMN c DATETIME DEFAULT LOCALTIMESTAMP", expectCol: "c"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cols, err := extractCurrentTimestampDefaultColumns(tc.query)
			require.NoError(t, err)
			require.Contains(t, cols, tc.expectCol,
				"%s should be detected as a timestamp default function", tc.name)
		})
	}
}

func TestExecDDL_DoesNotSetTimestampWhenNoCurrentTimestampDefault(t *testing.T) {
	writer, db, mock := newTestMysqlWriter(t)
	defer db.Close()
	writer.cfg.Timezone = "\"UTC\""

	helper := commonEvent.NewEventTestHelperWithTimeZone(t, time.UTC)
	defer helper.Close()

	helper.Tk().MustExec("use test")
	helper.DDL2Event("create table t (id int primary key)")

	ddlEvent := helper.DDL2Event("alter table t add column age int default 1")
	_, ok := ddlSessionTimestampForTest(ddlEvent, writer.cfg.Timezone)
	require.False(t, ok)

	mock.ExpectBegin()
	mock.ExpectExec("USE `test`;").WillReturnResult(sqlmock.NewResult(1, 1))
	expectDDLExec(mock, ddlEvent, writer.cfg.Timezone)
	mock.ExpectCommit()

	err := writer.execDDL(ddlEvent)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

package db

import (
	"fmt"

	"go.uber.org/zap"
)

// Insert a row in the DB, it is assumed the table exists, you can do a
// check before with HasTable()
func (l *Loader) Insert(tableName string, primaryKey map[string]string, data map[string]string) error {
	uniqueId := createKeyValuePairs(primaryKey)

	if l.tracer.Enabled() {
		l.logger.Debug("processing insert operation", zap.String("table_name", tableName), zap.String("primary_key", uniqueId), zap.Int("field_count", len(data)))
	}

	if _, found := l.entries[tableName]; !found {
		if l.tracer.Enabled() {
			l.logger.Debug("adding tracking of table never seen before", zap.String("table_name", tableName))
		}

		l.entries[tableName] = map[string]*Operation{}
	}

	if _, found := l.entries[tableName][uniqueId]; found {
		return fmt.Errorf("attempting to insert in table %q a primary key %q, that is already scheduled for insertion, insert should only be called once for a given primary key", tableName, primaryKey)
	}

	if l.tracer.Enabled() {
		l.logger.Debug("primary key entry never existed for table, adding insert operation", zap.String("primary_key", uniqueId), zap.String("table_name", tableName))
	}

	// we need to make sure to add the primary key in the data so that
	// it gets created
	for _, table := range l.tablePrimaryKeys[tableName] {
		data[table] = primaryKey[table]
	}
	l.entries[tableName][uniqueId] = l.newInsertOperation(tableName, primaryKey, data)
	l.entriesCount++
	return nil
}
func createKeyValuePairs(m map[string]string) string {
	return fmt.Sprint(m)
}

func (l *Loader) GetPrimaryKey(tableName string, pk string) (map[string]string, error) {
	primaryKeyColumns := l.tablePrimaryKeys[tableName]
	if len(primaryKeyColumns) > 1 {
		return nil, fmt.Errorf("table %q has composite primary key", tableName)
	}
	primaryKey := map[string]string{}
	for _, column := range primaryKeyColumns {
		primaryKey[column] = pk
	}

	return primaryKey, nil
}

// Update a row in the DB, it is assumed the table exists, you can do a
// check before with HasTable()
func (l *Loader) Update(tableName string, primaryKey map[string]string, data map[string]string) error {

	uniqueId := createKeyValuePairs(primaryKey)
	if l.tracer.Enabled() {
		l.logger.Debug("processing update operation", zap.String("table_name", tableName), zap.String("primary_key", uniqueId), zap.Int("field_count", len(data)))
	}

	if _, found := l.entries[tableName]; !found {
		if l.tracer.Enabled() {
			l.logger.Debug("adding tracking of table never seen before", zap.String("table_name", tableName))
		}

		l.entries[tableName] = map[string]*Operation{}
	}

	if op, found := l.entries[tableName][uniqueId]; found {
		if op.opType == OperationTypeDelete {
			return fmt.Errorf("attempting to update an object with primary key %q, that schedule to be deleted", primaryKey)
		}

		if l.tracer.Enabled() {
			l.logger.Debug("primary key entry already exist for table, merging fields together", zap.String("primary_key", uniqueId), zap.String("table_name", tableName))
		}

		op.mergeData(data)
		l.entries[tableName][uniqueId] = op
		return nil
	} else {
		l.entriesCount++
	}

	if l.tracer.Enabled() {
		l.logger.Debug("primary key entry never existed for table, adding update operation", zap.String("primary_key", uniqueId), zap.String("table_name", tableName))
	}

	l.entries[tableName][uniqueId] = l.newUpdateOperation(tableName, primaryKey, data)
	return nil
}

// Delete a row in the DB, it is assumed the table exists, you can do a
// check before with HasTable()
func (l *Loader) Delete(tableName string, primaryKey map[string]string) error {
	uniqueId := createKeyValuePairs(primaryKey)
	if l.tracer.Enabled() {
		l.logger.Debug("processing delete operation", zap.String("table_name", tableName), zap.String("primary_key", uniqueId))
	}

	if _, found := l.entries[tableName]; !found {
		if l.tracer.Enabled() {
			l.logger.Debug("adding tracking of table never seen before", zap.String("table_name", tableName))
		}

		l.entries[tableName] = map[string]*Operation{}
	}

	if _, found := l.entries[tableName][uniqueId]; !found {
		if l.tracer.Enabled() {
			l.logger.Debug("primary key entry never existed for table", zap.String("primary_key", uniqueId), zap.String("table_name", tableName))
		}

		l.entriesCount++
	}

	if l.tracer.Enabled() {
		l.logger.Debug("adding deleting operation", zap.String("primary_key", uniqueId), zap.String("table_name", tableName))
	}

	l.entries[tableName][uniqueId] = l.newDeleteOperation(tableName, primaryKey)
	return nil
}

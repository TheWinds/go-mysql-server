package analyzer

import (
	"fmt"
	"reflect"

	"github.com/dolthub/go-mysql-server/sql/plan"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/expression"
)

type filtersByTable map[string][]sql.Expression

func (f filtersByTable) merge(f2 filtersByTable) {
	for k, exprs := range f2 {
		f[k] = append(f[k], exprs...)
	}
}

// getFiltersByTable returns a map of table name to filter expressions on that table for the node provided. Returns an
// error only the case that the filters contained in the node given cannot all be separated into tables (some of them
// have more than one table, or no table)
func getFiltersByTable(_ *sql.Context, n sql.Node) (filtersByTable, error) {
	filters := make(filtersByTable)
	var err error
	plan.Inspect(n, func(node sql.Node) bool {
		if err != nil {
			return false
		}

		switch node := node.(type) {
		case *plan.Filter:
			var fs filtersByTable
			fs, err = exprToTableFilters(node.Expression)
			if err != nil {
				return false
			}
			filters.merge(fs)
		}
		return true
	})

	if err != nil {
		return nil, err
	}

	return filters, err
}

// exprToTableFilters returns a map of table name to filter expressions on that table for all parts of the expression
// given, split at AND. Returns an error only the case that the expressions cannot all be separated into tables (some
// of them have more than one table, or no table)
func exprToTableFilters(expr sql.Expression) (filtersByTable, error) {
	filtersByTable := make(filtersByTable)
	for _, expr := range splitConjunction(expr) {
		var seenTables = make(map[string]bool)
		var lastTable string
		sql.Inspect(expr, func(e sql.Expression) bool {
			f, ok := e.(*expression.GetField)
			if ok {
				if !seenTables[f.Table()] {
					seenTables[f.Table()] = true
					lastTable = f.Table()
				}
			}

			return true
		})

		if len(seenTables) == 1 {
			filtersByTable[lastTable] = append(filtersByTable[lastTable], expr)
		} else {
			return nil, fmt.Errorf("didn't find table for expression %s", expr.String())
		}
	}

	return filtersByTable, nil
}

type filterSet struct {
	filtersByTable      filtersByTable
	handledFilters      []sql.Expression
	handledIndexFilters []string
	aliases             ExprAliases
	tableAliases        TableAliases
}

// newFilterSet returns a new filter set that will track available filters with the filters and aliases given. Aliases
// are necessary to normalize expressions from indexes when in the presence of aliases.
func newFilterSet(filtersByTable filtersByTable, aliases ExprAliases, tableAliases TableAliases) *filterSet {
	return &filterSet{
		filtersByTable: filtersByTable,
		aliases: aliases,
		tableAliases: tableAliases,
	}
}

// availableFiltersForTable returns the filters that are still available for the table given (not previously marked
// handled)
func (fs *filterSet) availableFiltersForTable(table string) []sql.Expression {
	filters, ok := fs.filtersByTable[table]
	if !ok {
		return nil
	}
	return fs.subtractUsedIndexes(subtractExprSet(filters, fs.handledFilters))
}

// availableFilters returns the filters that are still available (not previously marked handled)
func (fs *filterSet) availableFilters() []sql.Expression {
	var available []sql.Expression
	for _, es := range fs.filtersByTable {
		available = append(available, fs.subtractUsedIndexes(subtractExprSet(es, fs.handledFilters))...)
	}
	return available
}

// handledCount returns the number of filter expressions that have been marked as handled
func (fs *filterSet) handledCount() int {
	return len(fs.handledIndexFilters) + len(fs.handledFilters)
}

// markFilterUsed marks the filter given as handled, so it will no longer be returned by availableFiltersForTable
func (fs *filterSet) markFiltersHandled(exprs ...sql.Expression) {
	fs.handledFilters = append(fs.handledFilters, exprs...)
}

// markIndexesHandled marks the indexes given as handled, so expressions on them will no longer be returned by
// availableFiltersForTable
func (fs *filterSet) markIndexesHandled(indexes []sql.Index) {
	for _, index := range indexes {
		fs.handledIndexFilters = append(fs.handledIndexFilters, index.Expressions()...)
	}
}

// splitConjunction breaks AND expressions into their left and right parts, recursively
func splitConjunction(expr sql.Expression) []sql.Expression {
	and, ok := expr.(*expression.And)
	if !ok {
		return []sql.Expression{expr}
	}

	return append(
		splitConjunction(and.Left),
		splitConjunction(and.Right)...,
	)
}

// subtractExprSet returns all expressions in the first parameter that aren't present in the second.
func subtractExprSet(all, toSubtract []sql.Expression) []sql.Expression {
	var remainder []sql.Expression

	for _, e := range all {
		var found bool
		for _, s := range toSubtract {
			if reflect.DeepEqual(e, s) {
				found = true
				break
			}
		}

		if !found {
			remainder = append(remainder, e)
		}
	}

	return remainder
}

// subtractUsedIndexes returns the filter expressions given with used indexes subtracted off.
func (fs *filterSet) subtractUsedIndexes(all []sql.Expression) []sql.Expression {
	var remainder []sql.Expression

	// Careful: index expressions are always normalized (contain actual table names), whereas filter expressions can
	// contain aliases for both expressions and table names. We want to normalize all expressions for comparison, but
	// return the original expressions.
	normalized := normalizeExpressions(fs.aliases, fs.tableAliases, all...)

	for i, e := range normalized {
		var found bool

		cmpStr := e.String()
		comparable, ok := e.(expression.Comparer)
		if ok {
			left, right := comparable.Left(), comparable.Right()
			if _, ok := left.(*expression.GetField); ok {
				cmpStr = left.String()
			} else {
				cmpStr = right.String()
			}
		}

		for _, s := range fs.handledIndexFilters {
			if cmpStr == s {
				found = true
				break
			}
		}

		if !found {
			remainder = append(remainder, all[i])
		}
	}

	return remainder
}
package lsp

import (
	"context"
	"fmt"
	"sync"

	"github.com/skflowne/portolan/internal/core"
)

const maxConcurrentProviderRequests = 8

type definitionSnapshot struct {
	symbols []core.SymbolNode
	source  string
}

type definitionSnapshotLoader func(context.Context, string) (definitionSnapshot, error)

func (p *Provider) DefinitionSources(ctx context.Context, locations []core.Location) ([]core.Definition, error) {
	return enrichDefinitionSources(ctx, locations, func(ctx context.Context, file string) (definitionSnapshot, error) {
		symbols, err := p.DocumentSymbols(ctx, file)
		if err != nil {
			return definitionSnapshot{}, err
		}
		source, ok := p.openedSource(file)
		if !ok {
			return definitionSnapshot{}, fmt.Errorf("lsp: opened source unavailable for %s", file)
		}
		return definitionSnapshot{symbols: symbols, source: source}, nil
	})
}

type definitionFileGroup struct {
	file    string
	indexes []int
}

func enrichDefinitionSources(ctx context.Context, locations []core.Location, load definitionSnapshotLoader) ([]core.Definition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(locations) == 0 {
		return []core.Definition{}, nil
	}

	groups := make([]definitionFileGroup, 0)
	groupIndexes := make(map[string]int)
	for i, location := range locations {
		groupIndex, exists := groupIndexes[location.File]
		if !exists {
			groupIndex = len(groups)
			groupIndexes[location.File] = groupIndex
			groups = append(groups, definitionFileGroup{file: location.File})
		}
		groups[groupIndex].indexes = append(groups[groupIndex].indexes, i)
	}

	definitions := make([]core.Definition, len(locations))
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var workers sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	recordError := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	workerCount := min(len(groups), maxConcurrentProviderRequests)
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for groupIndex := range jobs {
				group := groups[groupIndex]
				snapshot, err := load(workCtx, group.file)
				if err == nil {
					err = workCtx.Err()
				}
				if err == nil {
					err = enrichDefinitionGroup(workCtx, locations, group, snapshot, definitions)
				}
				if err != nil {
					recordError(err)
					return
				}
			}
		}()
	}
	for i := range groups {
		select {
		case jobs <- i:
		case <-workCtx.Done():
			break
		}
		if workCtx.Err() != nil {
			break
		}
	}
	close(jobs)
	workers.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return definitions, nil
}

func enrichDefinitionGroup(ctx context.Context, locations []core.Location, group definitionFileGroup, snapshot definitionSnapshot, definitions []core.Definition) error {
	for _, index := range group.indexes {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := locations[index]
		symbol, matches, err := exactDefinitionSymbol(ctx, snapshot.symbols, target.Range)
		if err != nil {
			return err
		}
		switch matches {
		case 0:
			return fmt.Errorf("lsp: definition target %s %+v does not match a declaration", target.File, target.Range)
		case 1:
		default:
			return fmt.Errorf("lsp: definition target %s %+v matches multiple declarations", target.File, target.Range)
		}
		source, err := textInRange(ctx, snapshot.source, symbol.Range)
		if err != nil {
			return fmt.Errorf("lsp: extracting declaration at %s %+v: %w", target.File, symbol.Range, err)
		}
		definitions[index] = core.Definition{
			Target:           target,
			DeclarationRange: symbol.Range,
			Source:           source,
		}
	}
	return nil
}

func exactDefinitionSymbol(ctx context.Context, symbols []core.SymbolNode, target core.Range) (core.Symbol, int, error) {
	var matched core.Symbol
	matches := 0
	var visit func([]core.SymbolNode) error
	visit = func(nodes []core.SymbolNode) error {
		for _, node := range nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if node.SelRange == target || node.Range == target {
				matched = node.Symbol
				matches++
			}
			if err := visit(node.Children); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(symbols); err != nil {
		return core.Symbol{}, 0, err
	}
	return matched, matches, nil
}

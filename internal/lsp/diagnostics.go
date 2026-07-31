package lsp

import (
	"bytes"

	"github.com/amirhasanzadehpy/Pogo/internal/analysis"
	"github.com/amirhasanzadehpy/Pogo/internal/schema"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

const diagnosticSource = "pogo"

func (features *Features) setNotifier(ctx *glsp.Context) {
	if features == nil || ctx == nil || ctx.Notify == nil {
		return
	}
	features.notifyMu.Lock()
	if !features.closed && features.notify == nil {
		features.notify = ctx.Notify
	}
	features.notifyMu.Unlock()
}

func (features *Features) SetNotifier(notify glsp.NotifyFunc) {
	if features == nil {
		return
	}
	features.notifyMu.Lock()
	if !features.closed {
		features.notify = notify
	}
	features.notifyMu.Unlock()
}

func (features *Features) sendNotification(method string, params any) {
	features.notifyMu.RLock()
	notify := features.notify
	closed := features.closed
	features.notifyMu.RUnlock()
	if !closed && notify != nil {
		notify(method, params)
	}
}

func (features *Features) save(uri string, text *string) error {
	if err := features.documents.Save(uri, text); err != nil {
		return err
	}
	features.publishURI(uri)
	return nil
}

func (features *Features) publishURI(uri string) {
	snapshot, ok := features.documents.Snapshot(uri)
	if !ok {
		return
	}
	graph, generation := features.cache.Load()
	features.publishSnapshot(snapshot, graph, generation)
}

func (features *Features) RevalidateAll(generation uint64) {
	graph, currentGeneration := features.cache.Load()
	if graph == nil || currentGeneration != generation {
		return
	}
	for _, snapshot := range features.documents.Snapshots() {
		features.publishSnapshot(snapshot, graph, generation)
	}
}

func (features *Features) publishSnapshot(snapshot analysis.Snapshot, graph *schema.Graph, generation uint64) {
	diagnostics := make([]protocol.Diagnostic, 0)
	for _, issue := range analysis.DiagnoseORM(snapshot, graph) {
		range_, ok := protocolRange(snapshot.Source, issue.Range)
		if !ok {
			continue
		}
		severity := protocol.DiagnosticSeverityError
		code := protocol.IntegerOrString{Value: string(issue.Code)}
		source := diagnosticSource
		diagnostics = append(diagnostics, protocol.Diagnostic{
			Range: range_, Severity: &severity, Code: &code, Source: &source, Message: issue.Message,
		})
	}
	features.diagnosticMu.Lock()
	defer features.diagnosticMu.Unlock()
	current, open := features.documents.Snapshot(snapshot.URI)
	_, currentGeneration := features.cache.Load()
	if !open || current.Version != snapshot.Version || !bytes.Equal(current.Source, snapshot.Source) || currentGeneration != generation {
		return
	}
	params := protocol.PublishDiagnosticsParams{URI: protocol.DocumentUri(snapshot.URI), Diagnostics: diagnostics}
	if snapshot.Version >= 0 {
		version := protocol.UInteger(snapshot.Version)
		params.Version = &version
	}
	features.sendNotification(string(protocol.ServerTextDocumentPublishDiagnostics), params)
}

func (features *Features) clearDiagnostics(uri string) {
	features.diagnosticMu.Lock()
	defer features.diagnosticMu.Unlock()
	features.sendNotification(string(protocol.ServerTextDocumentPublishDiagnostics), protocol.PublishDiagnosticsParams{
		URI: protocol.DocumentUri(uri), Diagnostics: make([]protocol.Diagnostic, 0),
	})
}

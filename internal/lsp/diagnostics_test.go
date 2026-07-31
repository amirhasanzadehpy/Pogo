package lsp

import (
	"strings"
	"testing"

	"github.com/amirhasanzadehpy/Pogo/internal/analysis"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

func TestDiagnosticPublicationOpenChangeClose(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	var publications []protocol.PublishDiagnosticsParams
	ctx := &glsp.Context{Notify: func(method string, params any) {
		if method != string(protocol.ServerTextDocumentPublishDiagnostics) {
			t.Fatalf("notification method = %q", method)
		}
		publications = append(publications, params.(protocol.PublishDiagnosticsParams))
	}}
	uri := protocol.DocumentUri("file:///diagnostics.py")
	invalid := "from myapp.models import Book\nlabel = '😀'; Book.objects.filter(author__missing=1)\n"
	if err := features.didOpen(ctx, &protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
		URI: uri, LanguageID: "python", Version: 1, Text: invalid,
	}}); err != nil {
		t.Fatal(err)
	}
	if len(publications) != 1 || publications[0].Version == nil || *publications[0].Version != 1 || len(publications[0].Diagnostics) != 1 {
		t.Fatalf("open publications = %#v", publications)
	}
	diagnostic := publications[0].Diagnostics[0]
	if diagnostic.Code == nil || diagnostic.Code.Value != string(analysis.IssueUnknownPathSegment) || diagnostic.Source == nil || *diagnostic.Source != "pogo" || diagnosticSource != "pogo" {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
	missing := strings.Index(invalid, "missing")
	wantStart, _ := analysis.PositionAt([]byte(invalid), missing)
	wantEnd, _ := analysis.PositionAt([]byte(invalid), missing+len("missing"))
	if diagnostic.Range.Start != protocolPosition(wantStart) || diagnostic.Range.End != protocolPosition(wantEnd) {
		t.Fatalf("diagnostic range = %#v, want %v..%v", diagnostic.Range, wantStart, wantEnd)
	}

	valid := strings.Replace(invalid, "missing", "name", 1)
	if err := features.didChange(ctx, &protocol.DidChangeTextDocumentParams{
		TextDocument:   protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: uri}, Version: 2},
		ContentChanges: []any{protocol.TextDocumentContentChangeEventWhole{Text: valid}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(publications) != 2 || publications[1].Version == nil || *publications[1].Version != 2 || publications[1].Diagnostics == nil || len(publications[1].Diagnostics) != 0 {
		t.Fatalf("change publication = %#v", publications[1])
	}

	if err := features.didClose(ctx, &protocol.DidCloseTextDocumentParams{TextDocument: protocol.TextDocumentIdentifier{URI: uri}}); err != nil {
		t.Fatal(err)
	}
	if len(publications) != 3 || publications[2].Version != nil || publications[2].Diagnostics == nil || len(publications[2].Diagnostics) != 0 {
		t.Fatalf("close publication = %#v", publications[2])
	}
}

func TestDiagnosticRevalidationUsesGenerationAndOpenDocuments(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	var publications []protocol.PublishDiagnosticsParams
	features.SetNotifier(func(_ string, params any) {
		publications = append(publications, params.(protocol.PublishDiagnosticsParams))
	})
	uri := "file:///revalidate.py"
	if err := features.documents.Open(uri, 7, "from myapp.models import Book\nBook.objects.filter(missing=1)\n"); err != nil {
		t.Fatal(err)
	}
	_, generation := features.cache.Load()
	features.RevalidateAll(generation)
	if len(publications) != 1 || publications[0].Version == nil || *publications[0].Version != 7 || len(publications[0].Diagnostics) != 1 {
		t.Fatalf("revalidation publications = %#v", publications)
	}
	features.documents.Close(uri)
	features.RevalidateAll(generation)
	if len(publications) != 1 {
		t.Fatalf("closed document was revalidated: %#v", publications)
	}
	features.RevalidateAll(generation + 1)
	if len(publications) != 1 {
		t.Fatalf("stale generation was revalidated: %#v", publications)
	}
}

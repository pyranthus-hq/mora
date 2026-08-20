package mora

import graphpkg "github.com/pyranthus-hq/mora/internal/graph"

type gazetteer = graphpkg.Gazetteer

func normalizeGazName(s string) (string, bool)        { return graphpkg.NormalizeGazName(s) }
func gazetteerScan(g gazetteer, text string) []string { return graphpkg.ScanGazetteer(g, text) }
func tokenizeWords(s string) []string                 { return graphpkg.TokenizeWords(s) }

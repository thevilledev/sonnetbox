package securejsonnet

import "context"

type EngineConfig struct {
	MaxMemoryBytes       uint64
	MaxSourceBytes       uint32
	MaxOutputBytes       uint32
	MaxStack             int
	MaxImports           uint32
	MaxImportBytes       uint32
	MaxTotalImportBytes  uint64
	MaxCapabilityCalls   uint32
	MaxHostRequestBytes  uint32
	MaxHostResponseBytes uint32
}

type Importer interface {
	Import(
		ctx context.Context,
		importedFrom string,
		importedPath string,
	) (canonicalPath string, content []byte, err error)
}

type Capability struct {
	Params []string
	Call   func(context.Context, []any) (any, error)
}

type Request struct {
	Filename     string
	Source       string
	ExtVars      map[string]string
	ExtCode      map[string]string
	TLAVars      map[string]string
	TLACode      map[string]string
	Importer     Importer
	Capabilities map[string]Capability
}

type Result struct {
	Output []byte
}

package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"vault/internal/metadata"
)

// ExecuteResult 描述适配器实际生成的单文件备份及恢复元数据。
type ExecuteResult struct {
	ProducedPath   string
	Extension      string
	Method         string
	EngineMetadata json.RawMessage
}

// BackupSpec 声明本次运行要执行的备份类型。
// 差异与日志必须显式声明；不支持的引擎要明确失败，不能降级成全量冒充。
type BackupSpec struct {
	Type string // full | differential | log
}

// Adapter 是真实数据库备份适配器；是否支持差异或日志由各引擎实现决定。
type Adapter interface {
	Engine() string
	Version(context.Context) (string, error)
	Probe(context.Context, metadata.Source, string) (string, error)
	Execute(context.Context, metadata.Source, metadata.StorageTarget, string, string, BackupSpec) (ExecuteResult, error)
}

// Registry 按数据源引擎选择真实适配器，未知引擎不会被默认放行。
type Registry struct {
	adapters map[string]Adapter
}

// NewRegistry 注册当前实现的引擎适配器。
func NewRegistry(adapters ...Adapter) *Registry {
	registry := &Registry{adapters: make(map[string]Adapter, len(adapters))}
	for _, adapter := range adapters {
		registry.adapters[adapter.Engine()] = adapter
	}
	return registry
}

// ForEngine 返回引擎适配器；未实现引擎返回明确错误。
func (r *Registry) ForEngine(engine string) (Adapter, error) {
	adapter, ok := r.adapters[engine]
	if !ok {
		return nil, fmt.Errorf("引擎 %s 的备份适配器未实现", engine)
	}
	return adapter, nil
}

// Engines 返回稳定排序的已实现引擎列表。
func (r *Registry) Engines() []string {
	engines := make([]string, 0, len(r.adapters))
	for engine := range r.adapters {
		engines = append(engines, engine)
	}
	sort.Strings(engines)
	return engines
}

package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"dbbak/internal/metadata"
)

// ExecuteResult 描述适配器实际生成的备份对象及恢复元数据。
// ProducedPath 为空表示产物不在执行器磁盘上（如 SQL Server 写入数据库服务器目录）。
type ExecuteResult struct {
	ProducedPath   string
	Extension      string
	Method         string
	EngineMetadata json.RawMessage
}

// BackupSpec 声明本次运行要执行的备份类型与恢复链基线。
//
// Base 只对差异、增量与日志路线有意义：适配器据此确定增量基线或日志起点，
// 全量路线忽略该字段。把基线交给适配器而不是让它自行查询元数据，
// 是为了让「用哪一份基线」这个决定在执行器里只做一次，避免两处口径不一致。
type BackupSpec struct {
	Type string // full | differential | log
	Base *metadata.Artifact
	// FileName 是本次备份的目标文件名（不含扩展名与路径），由 runner 按
	// <数据库名>-<YYYYMMDDHHmmss> 生成。需要由数据库服务自己写盘的适配器
	// （SQL Server 原生 BACKUP）必须用它命名，不能再用执行器临时目录名——
	// 那个名字是 os.MkdirTemp 生成的随机串，与备份对象毫无关系。
	FileName string
}

// RouteNamer 由同一引擎存在多条备份路线的适配器实现。
//
// 同一引擎可以同时存在逻辑路线与物理路线（如 MySQL 的 mysqldump 与 xtrabackup），
// 二者的恢复语义、平台前提和产物格式完全不同，必须显式区分而不能靠引擎名区分。
type RouteNamer interface {
	Method() string
}

// Adapter 是真实数据库备份适配器；是否支持差异或日志由各引擎实现决定。
type Adapter interface {
	Engine() string
	Version(context.Context) (string, error)
	Probe(context.Context, metadata.Source, string) (string, error)
	Execute(context.Context, metadata.Source, metadata.StorageTarget, string, string, BackupSpec) (ExecuteResult, error)
}

// Route 是一条可执行的备份路线，用于能力矩阵展示与任务校验。
type Route struct {
	Engine  string
	Method  string
	Adapter Adapter
}

// Registry 按「引擎 + 路线」选择真实适配器，未知组合不会被默认放行。
type Registry struct {
	// routes 的键为 routeKey(engine, method)。
	routes map[string]Adapter
	// defaults 是引擎未指定路线时的落点；空的路线名表示该引擎沿用历史默认行为。
	defaults map[string]Adapter
}

// NewRegistry 注册当前实现的引擎适配器。
//
// 未实现 RouteNamer 的适配器只有默认路线；实现了的适配器除默认路线外还可被显式点名。
// 默认路线取第一个未声明路线的适配器，否则取第一个注册的适配器，
// 因此装配顺序决定 mysql 的默认路线仍是逻辑全量。
func NewRegistry(adapters ...Adapter) *Registry {
	registry := &Registry{
		routes:   make(map[string]Adapter, len(adapters)),
		defaults: make(map[string]Adapter, len(adapters)),
	}
	for _, adapter := range adapters {
		method := RouteMethod(adapter)
		registry.routes[routeKey(adapter.Engine(), method)] = adapter
		if _, exists := registry.defaults[adapter.Engine()]; !exists || method == "" {
			// 未声明路线的适配器优先作为默认，保证既有 MySQL 逻辑全量行为不变。
			registry.defaults[adapter.Engine()] = adapter
		}
	}
	return registry
}

// RouteMethod 返回适配器声明的路线名；未实现 RouteNamer 时返回空串。
func RouteMethod(adapter Adapter) string {
	if namer, ok := adapter.(RouteNamer); ok {
		return namer.Method()
	}
	return ""
}

// ForRoute 返回指定引擎与路线的适配器。
//
// method 为空表示使用该引擎的默认路线；显式指定但未实现时直接报错，
// 不允许回退到其他路线，否则配置错误会被静默执行成另一种备份。
func (r *Registry) ForRoute(engine, method string) (Adapter, error) {
	if adapter, ok := r.routes[routeKey(engine, method)]; ok {
		return adapter, nil
	}
	if method != "" {
		return nil, fmt.Errorf("引擎 %s 的 %s 备份路线未实现", engine, method)
	}
	if adapter, ok := r.defaults[engine]; ok {
		return adapter, nil
	}
	return nil, fmt.Errorf("引擎 %s 的备份适配器未实现", engine)
}

// ForEngine 返回引擎的默认路线适配器；未实现引擎返回明确错误。
func (r *Registry) ForEngine(engine string) (Adapter, error) {
	return r.ForRoute(engine, "")
}

// Engines 返回稳定排序的已实现引擎列表（同一引擎只出现一次）。
func (r *Registry) Engines() []string {
	seen := make(map[string]struct{}, len(r.routes))
	engines := make([]string, 0, len(r.routes))
	for _, adapter := range r.routes {
		engine := adapter.Engine()
		if _, ok := seen[engine]; ok {
			continue
		}
		seen[engine] = struct{}{}
		engines = append(engines, engine)
	}
	sort.Strings(engines)
	return engines
}

// Routes 返回全部已实现路线，按引擎名与路线名排序，供能力矩阵逐条展示。
func (r *Registry) Routes() []Route {
	routes := make([]Route, 0, len(r.routes))
	for _, adapter := range r.routes {
		routes = append(routes, Route{Engine: adapter.Engine(), Method: RouteMethod(adapter), Adapter: adapter})
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Engine != routes[j].Engine {
			return routes[i].Engine < routes[j].Engine
		}
		return routes[i].Method < routes[j].Method
	})
	return routes
}

func routeKey(engine, method string) string { return engine + "\x00" + method }

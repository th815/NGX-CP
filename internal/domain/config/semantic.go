package config

import (
	"context"
	"fmt"

	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/internal/domain/config/rules"
)

// SemanticChecker 把「节点当前配置 + 编译能力 + 同集群 peer 能力」组装为规则引擎输入，
// 运行语义校验。证书引用检查依赖节点文件清单（KnownFiles），当前 Agent 尚未上报，
// 故 KnownFiles 暂为空（CertRefRule 自动跳过，待 T018 文件清单补齐后生效）。
type SemanticChecker struct {
	client *ent.Client
	store  *ConfigStore
	cfg    *rules.Config
}

// NewSemanticChecker 构造语义校验器。cfg 为 nil 时使用内建默认规则配置。
func NewSemanticChecker(client *ent.Client, store *ConfigStore, cfg *rules.Config) *SemanticChecker {
	if cfg == nil {
		cfg = rules.DefaultConfig()
	}
	return &SemanticChecker{client: client, store: store, cfg: cfg}
}

// Check 对指定节点运行语义校验，返回所有启用的规则检出。
func (c *SemanticChecker) Check(ctx context.Context, nodeID int) ([]rules.Issue, error) {
	node, err := c.client.Node.Get(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("查询节点 %d 失败: %w", nodeID, err)
	}

	in := &rules.CheckInput{
		Node: &rules.Node{ID: node.ID, Name: node.Name, Role: string(node.Role)},
	}
	// 编译能力基线。
	if caps, e := node.QueryCapabilities().Only(ctx); e == nil && caps != nil {
		in.Capability = &rules.Capability{Modules: caps.Modules}
	}

	// 当前配置树（每个文件取当前版本内容）。
	files, err := c.store.ListFiles(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("列举配置文件失败: %w", err)
	}
	for _, f := range files {
		content, err := c.store.GetCurrentContent(ctx, f.ID)
		if err != nil {
			continue
		}
		in.ConfigFiles = append(in.ConfigFiles, rules.ConfigFile{Path: f.Path, Content: string(content)})
	}

	// 同集群 peer 能力（双机一致性比对用）。
	if cl, e := node.QueryCluster().Only(ctx); e == nil && cl != nil {
		if peers, e2 := cl.QueryNodes().All(ctx); e2 == nil {
			for _, p := range peers {
				if p.ID == nodeID {
					continue
				}
				peer := rules.Peer{Name: p.Name, Role: string(p.Role)}
				if pc, e3 := p.QueryCapabilities().Only(ctx); e3 == nil && pc != nil {
					peer.Cap = &rules.Capability{Modules: pc.Modules}
				}
				in.Peers = append(in.Peers, peer)
			}
		}
	}

	engine := rules.NewEngine(c.cfg)
	return engine.Check(ctx, in), nil
}

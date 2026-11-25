package plugin

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// 用户 2026-11-25：全面复查。宿主的 reconfigure 会在运行中再次调用
// register 生命周期。旧实现每次都会 Load + AdoptStore，导致：
//  1. 旧 store 的 auto-flush goroutine 继续每 5s 写 state.json，
//     用旧快照覆盖新 store 的数据（新 store 反而没有 flush 循环）；
//  2. 运行期窗口/绑定/新账号在重启后丢失。
//
// reconfigure 必须复用同一个 store，不得重新加载。
func TestReconfigureKeepsStore(t *testing.T) {
	host := &fakeHost{}
	p, err := New(host, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})
	before := p.Store()
	if before == nil {
		t.Fatal("store nil after register")
	}

	id := addAccount(t, p, "keep@x.com", "oc_sk_keep_00000000000000000000")
	until := time.Now().Add(2 * time.Hour)
	p.Store().ObserveWindowSource(id, "weekly", "429", 100, until, time.Now())

	// 宿主运行中 reconfigure（同一插件实例再次 register）。
	cfg := base64.StdEncoding.EncodeToString([]byte("upstream_user_agent: test/9.9"))
	call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": cfg})

	if p.Store() != before {
		t.Fatal("reconfigure 不得替换 store（会导致旧 flush 覆盖新数据、状态丢失）")
	}
	acc := p.Store().Snapshot().Accounts[id]
	if acc == nil || acc.Windows["weekly"] == nil {
		t.Fatalf("reconfigure 后窗口丢失：%+v", acc)
	}
	if got := p.config().UpstreamUserAgent; got != "test/9.9" {
		t.Fatalf("reconfigure 应更新配置：got %q", got)
	}
}

package postgres

// SetFaultHook 仅供测试注入事务内故障，返回恢复原钩子的函数。
// 生产运行时不设置钩子，Push 中的 injectFault 直接返回 nil。
func SetFaultHook(h func(stage string) error) func() {
	prev := faultHook
	faultHook = h
	return func() { faultHook = prev }
}

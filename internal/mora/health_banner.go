package mora

import healthpkg "github.com/pyranthus-hq/mora/internal/health"

const healthBannerLineCap = healthpkg.BannerLineCap

func healthBannerFrom(h Health) string { return healthpkg.BannerAll(h) }
func capBannerLine(s string) string    { return healthpkg.CapBannerLine(s) }

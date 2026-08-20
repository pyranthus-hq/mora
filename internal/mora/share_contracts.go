package mora

import sharingpkg "github.com/pyranthus-hq/mora/internal/sharing"

const shareFileSchema = sharingpkg.LedgerSchema

type shareFile = sharingpkg.Ledger
type sharePublish = sharingpkg.Publish
type shareSubscription = sharingpkg.Subscription
type transportRef = sharingpkg.TransportRef
type bucketConfig = sharingpkg.BucketConfig

func validShareName(s string) bool                   { return sharingpkg.ValidName(s) }
func validShareScope(s string) bool                  { return sharingpkg.ValidScope(s) }
func sharesPath(cfg Config) string                   { return sharingpkg.LedgerPath(cfg.ConfigDir) }
func shareStagingDir(cfg Config, name string) string { return sharingpkg.StagingDir(cfg.DataDir, name) }
func shareSubRoot(cfg Config, name string) string {
	return sharingpkg.SubscriptionRoot(cfg.DataDir, name)
}
func shareRepoDir(cfg Config, name string) string   { return sharingpkg.RepoDir(cfg.DataDir, name) }
func shareCorpusDir(cfg Config, name string) string { return sharingpkg.CorpusDir(cfg.DataDir, name) }
func shareIndexPath(cfg Config, name string) string { return sharingpkg.IndexPath(cfg.DataDir, name) }
func loadShares(cfg Config) (shareFile, error)      { return sharingpkg.Load(cfg.ConfigDir) }
func saveShares(cfg Config, ledger shareFile) error { return sharingpkg.Save(cfg.ConfigDir, ledger) }
func validateSubscriptionNameAvailable(ledger shareFile, name string) error {
	return sharingpkg.ValidateSubscriptionNameAvailable(ledger, name)
}

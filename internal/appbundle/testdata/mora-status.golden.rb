# typed: strict
# frozen_string_literal: true

cask "mora-status" do
  version "1.0.0"
  sha256 "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

  url "https://github.com/pyranthus-hq/mora/releases/download/mora-status-v#{version}/mora-status_#{version}_arm64.zip"
  name "Mora Status"
  desc "Menu bar status and agent-note review for Mora"
  homepage "https://github.com/pyranthus-hq/mora"

  depends_on arch: :arm64
  depends_on macos: :ventura

  app "Mora Status.app"

  caveats <<~EOS
    Mora Status needs the mora command. It looks in ~/.local/bin, Homebrew, and Mora.app.
    To use another vault: defaults write com.pyranthus.mora-status vault /absolute/path
    Uninstalling removes only the app; your vault and Mora settings stay.
  EOS
end

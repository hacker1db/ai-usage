class FoundryEntraProxy < Formula
  desc "Loopback proxy that signs Azure AI Foundry requests with an Entra token"
  homepage "https://github.com/hacker1db/ai-usage/blob/main/docs/foundry-entra-proxy.md"
  url "https://github.com/hacker1db/ai-usage/archive/refs/tags/v0.2.0.tar.gz"
  sha256 "6770d1c1e30266011e453c8bb3e07d5132e185a61a40a37c2ba29a8fac1233cd"
  head "https://github.com/hacker1db/ai-usage.git", branch: "main"

  depends_on "go" => :build

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=v#{version}"), "./cmd/foundry-entra-proxy"
  end

  def caveats
    <<~EOS
      The proxy is configured through FEP_* environment variables and needs the Azure CLI
      (az) signed in. To run it as a per-user LaunchAgent, use scripts/install-proxy.sh from
      the ai-usage repo; it uses this Homebrew binary when it is installed.
    EOS
  end

  test do
    assert_match "v#{version}", shell_output("#{bin}/foundry-entra-proxy -version")
  end
end

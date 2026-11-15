class AiUsage < Formula
  desc "Local dashboard and reports for AI coding tool usage"
  homepage "https://github.com/hacker1db/ai-usage"
  url "https://github.com/hacker1db/ai-usage/archive/refs/tags/v0.2.0.tar.gz"
  sha256 "6770d1c1e30266011e453c8bb3e07d5132e185a61a40a37c2ba29a8fac1233cd"
  head "https://github.com/hacker1db/ai-usage.git", branch: "main"

  depends_on "go" => :build

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=v#{version}"), "./cmd/ai-usage"
  end

  test do
    assert_match "v#{version}", shell_output("#{bin}/ai-usage version")
  end
end

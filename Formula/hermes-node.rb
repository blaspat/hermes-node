class HermesNode < Formula
  desc "Standalone Go binary that pairs a remote laptop with a Hermes Agent brain over WSS"
  homepage "https://github.com/blaspat/hermes-nodes"
  url "https://github.com/blaspat/hermes-nodes/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "503792d39339420fda6951622d848a470bf690238623dfd9a5573b1148ee4193"
  license "MIT"

  depends_on "go" => :build

  def install
    ldflags = "-s -w -X main.version=#{version}"
    system "go", "build", *std_go_args(ldflags: ldflags), "./cmd/hermes-node"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/hermes-node --version")
  end
end

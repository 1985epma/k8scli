class K8scli < Formula
  desc "Kubernetes CLI helper tool"
  homepage "https://github.com/1985epma/k8scli"
  url "https://github.com/1985epma/k8scli/archive/refs/heads/main.tar.gz"
  sha256 "908c906fa60b7ea6ba511222bc21d69f108919575d6ae41265610e127b9c3cc5"
  version "0.0.0"
  license "MIT"

  depends_on "go" => :build

  def install
    source_dir = buildpath.children.find { |entry| entry.directory? && (entry/"go.mod").exist? } || buildpath

    cd source_dir do
      system "go", "build", *std_go_args(ldflags: "-s -w"), "."
    end

    generate_completions_from_executable(bin/"k8scli", "completion")
  end

  test do
    assert_match "K8sCLI - Kubernetes CLI Helper", shell_output("#{bin}/k8scli help")
  end
end
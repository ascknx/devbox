{
  description = "Instant, easy, predictable dev environments";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    { self
    , nixpkgs
    , flake-utils
    ,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        lastTag = "0.14.2";

        revision = if (self ? shortRev) then "${self.shortRev}" else "${self.dirtyShortRev or "dirty"}";

        # Add the commit to the version string for flake builds
        version = "${lastTag}";

        # Run `devbox run update-flake` to update the vendor-hash
        vendorHash = if builtins.pathExists ./vendor-hash then builtins.readFile ./vendor-hash else "";

        buildGoModule = pkgs.buildGo124Module;

      in
      {
        inherit self;
        packages.default = buildGoModule {
          pname = "devbox";
          inherit version vendorHash;

          src = ./.;

          subpackage = [ ./cmd/devbox ];

          ldflags = [
            "-s"
            "-w"
            "-X go.jetify.com/devbox/internal/build.Version=${version}"
            "-X go.jetify.com/devbox/internal/build.Commit=${revision}"
          ];

          # Don't generate test binaries (as we'd include them as a bin)
          excludedPackages = [ "testscripts" ];

          # Disable tests if they require network access or are integration tests
          doCheck = false;

          nativeBuildInputs = [ pkgs.installShellFiles ];

          postInstall = pkgs.lib.optionalString (pkgs.stdenv.buildPlatform.canExecute pkgs.stdenv.hostPlatform) ''
            installShellCompletion --cmd devbox \
              --bash <($out/bin/devbox completion bash) \
              --fish <($out/bin/devbox completion fish) \
              --zsh <($out/bin/devbox completion zsh)
          '';

          meta = with pkgs.lib; {
            description = "Instant, easy, and predictable development environments";
            homepage = "https://www.jetify.com/devbox";
            license = licenses.asl20;
            maintainers = with maintainers; [ lagoja ];
          };
        };
      }
    );
}

{
  description = "kosh";
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

    systems.url = "github:nix-systems/default";

    gonixgo = {
      url = "github:draganm/gonixgo/v0.2.0";
      inputs.nixpkgs.follows = "nixpkgs";
      inputs.systems.follows = "systems";
    };
  };

  outputs = { self, nixpkgs, systems, gonixgo, ... }:
    let
      eachSystem = f:
        nixpkgs.lib.genAttrs (import systems)
        (system: f system nixpkgs.legacyPackages.${system});

      # What `kosh version` prints. A release is built with the tag in
      # KOSH_VERSION and --impure, without which Nix reads no environment;
      # every other build is named by its commit, with -dirty when the
      # tree had changes.
      version = let fromEnv = builtins.getEnv "KOSH_VERSION";
      in if fromEnv != "" then
        fromEnv
      else
        self.shortRev or self.dirtyShortRev or "dev";

      # The Go that builds kosh, in the derivation and in the shells: the
      # one go.mod asks for. go.mod says go 1.26.8, and the go_1_26 of the
      # locked nixpkgs is 1.26.8. When go.mod moves past the Go of this
      # nixpkgs, this moves with it, to go_1_27 or a newer nixpkgs.
      go = pkgs: pkgs.go_1_26;

      # The tools of both shells besides the program.
      tools = pkgs:
        with pkgs; [
          (go pkgs)
          kubernetes-helm
          kubeconform
          yq-go
          kind
          kubectl
        ];
    in {

      # Evaluating a gonixgo package runs `gonixgo resolve` through
      # builtins.exec, so every command that touches `packages` needs
      #   --option allow-unsafe-native-code-during-evaluation true
      packages = eachSystem (system: pkgs:
        let
          # This pkgs builds gonixgo's own tool and the Go program.
          #
          # The resolver runs on the machine that evaluates. That is this
          # system, unless a package of another system is asked for, as
          # the Linux image is on a Mac: then --impure tells which machine
          # evaluates, and its nixpkgs runs the resolver.
          evalPkgs = if builtins ? currentSystem && builtins.currentSystem
          != system then
            nixpkgs.legacyPackages.${builtins.currentSystem}
          else
            null;
          # The Go that resolves the packages while Nix evaluates has to
          # be the same one as the Go that builds.
          goEnv = gonixgo.lib.mkGoEnv {
            inherit pkgs evalPkgs;
            go = go pkgs.buildPackages;
            evalGo = if evalPkgs != null then go evalPkgs else null;
          };

          # The one program, without cgo.
          #
          # gonixgo asks `go list` while Nix evaluates which packages
          # cmd/kosh is made of, without build tags: cli/pin.go, which is
          # built only with the tag pin, is not among them.
          kosh = goEnv.buildGoApplication {
            pname = "kosh";
            src = ./.;
            subPackages = [ "cmd/kosh" ];
            CGO_ENABLED = 0;
            ldflags =
              [ "-X github.com/fables-for-robots/kosh/cli.Version=${version}" ];
            meta.license = pkgs.lib.licenses.lgpl3Only;
          };
        in { default = kosh; });

      # The shell carries the built program, so entering it (or direnv
      # reloading it after a source change, see .envrc) rebuilds it. It
      # therefore needs the same evaluation option as `packages`.
      devShells = eachSystem (system: pkgs: {
        default = pkgs.mkShell {
          hardeningDisable = [ "all" ];
          packages = tools pkgs ++ [ self.packages.${system}.default ];
        };

        # The same tools without the built program. The default shell
        # cannot be entered while the module does not resolve; this one
        # can. It needs no evaluation option, and is what the workflows
        # and a machine without Go run the tests in.
        bare = pkgs.mkShell {
          hardeningDisable = [ "all" ];
          packages = tools pkgs;
        };
      });
    };
}

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
      #
      # gonixgo splits ldflags on white space, so a version with a space
      # in it would become more arguments of the linker: KOSH_VERSION is
      # held to letters, digits and . _ + -.
      version = let fromEnv = builtins.getEnv "KOSH_VERSION";
      in if fromEnv == "" then
        self.shortRev or self.dirtyShortRev or "dev"
      else if builtins.match "[A-Za-z0-9._+-]+" fromEnv != null then
        fromEnv
      else
        throw "KOSH_VERSION is '${fromEnv}': a version is letters, digits and . _ + - only, as v1.2.3";

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

          # The one program. Without cgo it is linked statically on Linux,
          # and runs in an image that has no libc.
          #
          # gonixgo resolves the packages cmd/kosh is made of, asking
          # `go list` while Nix evaluates.
          kosh = goEnv.buildGoApplication {
            pname = "kosh";
            src = ./.;
            subPackages = [ "cmd/kosh" ];
            CGO_ENABLED = 0;
            ldflags =
              [ "-X github.com/fables-for-robots/kosh/cli.Version=${version}" ];
            meta.license = pkgs.lib.licenses.lgpl3Only;
          };
        in {
          default = kosh;
        } // nixpkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          # The image the chart runs:
          #   nix build .#image && docker load < result
          #
          # kosh, the certificates it reaches the relays and number0's DNS
          # with over HTTPS, and the user it runs as; no shell and no
          # other program. The program and the certificates are copied to
          # where an image has them, which works because both are files
          # that need no other.
          #
          # Nothing is in its /nix/store: left to itself Nix puts there
          # what the copies name, and the Go of nixpkgs writes into every
          # program where it would look for the time zones, the MIME types
          # and the names of protocols and ports when the machine has none
          # (tzdata, mailcap, iana-etc). kosh needs none of them, so the
          # image is made without store paths.
          image = pkgs.dockerTools.buildLayeredImage {
            name = "ghcr.io/fables-for-robots/kosh";
            tag = "nix";
            includeStorePaths = false;
            extraCommands = ''
              mkdir -p bin etc/ssl/certs
              mkdir -m 1777 tmp

              # Where the chart mounts the state and the key list.
              mkdir -p var/lib/kosh etc/kosh

              cp ${kosh}/bin/kosh bin/kosh

              cp ${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt etc/ssl/certs/ca-certificates.crt
              chmod 0444 etc/ssl/certs/ca-certificates.crt

              # The user kosh runs as, which the chart's securityContext
              # names by its number.
              echo 'kosh:x:65532:65532:kosh:/:/sbin/nologin' > etc/passwd
              echo 'kosh:x:65532:' > etc/group
            '';
            # The state directory is kosh's, so that a volume put there
            # without the chart's fsGroup starts out writable by it.
            # fakeroot records the owner in the layer and adds nothing to
            # the image.
            fakeRootCommands = ''
              chown 65532:65532 var/lib/kosh
            '';
            config = {
              Entrypoint = [ "/bin/kosh" ];
              User = "65532:65532";
              Env = [ "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt" ];
            };
          };
        });

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

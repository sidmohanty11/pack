//go:build acceptance
// +build acceptance

package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/buildpacks/lifecycle/api"
	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/ghodss/yaml"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/pelletier/go-toml"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/pack/acceptance/assertions"
	"github.com/buildpacks/pack/acceptance/buildpacks"
	"github.com/buildpacks/pack/acceptance/config"
	"github.com/buildpacks/pack/acceptance/invoke"
	"github.com/buildpacks/pack/acceptance/managers"
	"github.com/buildpacks/pack/internal/style"
	"github.com/buildpacks/pack/pkg/archive"
	"github.com/buildpacks/pack/pkg/cache"
	h "github.com/buildpacks/pack/testhelpers"
)

const (
	runImage   = "pack-test/run"
	buildImage = "pack-test/build"
)

var (
	dockerCli      client.CommonAPIClient
	registryConfig *h.TestRegistryConfig
	suiteManager   *SuiteManager
	imageManager   managers.ImageManager
	assertImage    assertions.ImageAssertionManager
)

func TestAcceptance(t *testing.T) {
	var err error

	h.RequireDocker(t)

	assert := h.NewAssertionManager(t)

	dockerCli, err = client.NewClientWithOpts(client.FromEnv, client.WithVersion("1.38"))
	assert.Nil(err)

	imageManager = managers.NewImageManager(t, dockerCli)

	registryConfig = h.RunRegistry(t)
	defer registryConfig.RmRegistry(t)

	assertImage = assertions.NewImageAssertionManager(t, imageManager, registryConfig)

	inputConfigManager, err := config.NewInputConfigurationManager()
	assert.Nil(err)

	assetsConfig := config.ConvergedAssetManager(t, assert, inputConfigManager)

	suiteManager = &SuiteManager{out: t.Logf}
	suite := spec.New("acceptance suite", spec.Report(report.Terminal{}))

	if inputConfigManager.Combinations().IncludesCurrentSubjectPack() {
		suite("p_current", func(t *testing.T, when spec.G, it spec.S) {
			testWithoutSpecificBuilderRequirement(
				t,
				when,
				it,
				assetsConfig.NewPackAsset(config.Current),
			)
		}, spec.Report(report.Terminal{}))
	}

	for _, combo := range inputConfigManager.Combinations() {
		// see https://github.com/golang/go/wiki/CommonMistakes#using-reference-to-loop-iterator-variable
		combo := combo

		t.Logf(`setting up run combination %s: %s`,
			style.Symbol(combo.String()),
			combo.Describe(assetsConfig),
		)

		suite(combo.String(), func(t *testing.T, when spec.G, it spec.S) {
			testAcceptance(
				t,
				when,
				it,
				assetsConfig.NewPackAsset(combo.Pack),
				assetsConfig.NewPackAsset(combo.PackCreateBuilder),
				assetsConfig.NewLifecycleAsset(combo.Lifecycle),
			)
		}, spec.Report(report.Terminal{}))
	}

	suite.Run(t)

	assert.Nil(suiteManager.CleanUp())
}

// These tests either (a) do not require a builder or (b) do not require a specific builder to be provided
// in order to test compatibility.
// They should only be run against the "current" (i.e., main) version of pack.
func testWithoutSpecificBuilderRequirement(
	t *testing.T,
	when spec.G,
	it spec.S,
	packConfig config.PackAsset,
) {
	var (
		pack             *invoke.PackInvoker
		assert           = h.NewAssertionManager(t)
		buildpackManager buildpacks.BuildModuleManager
	)

	it.Before(func() {
		pack = invoke.NewPackInvoker(t, assert, packConfig, registryConfig.DockerConfigDir)
		pack.EnableExperimental()
		buildpackManager = buildpacks.NewBuildModuleManager(t, assert)
	})

	it.After(func() {
		pack.Cleanup()
	})

	when("invalid subcommand", func() {
		it("prints usage", func() {
			output, err := pack.Run("some-bad-command")
			assert.NotNil(err)

			assertOutput := assertions.NewOutputAssertionManager(t, output)
			assertOutput.ReportsCommandUnknown("some-bad-command")
			assertOutput.IncludesUsagePrompt()
		})
	})

	when("build with default builders not set", func() {
		it("informs the user", func() {
			output, err := pack.Run(
				"build", "some/image",
				"-p", filepath.Join("testdata", "mock_app"),
			)

			assert.NotNil(err)
			assertOutput := assertions.NewOutputAssertionManager(t, output)
			assertOutput.IncludesMessageToSetDefaultBuilder()
			assertOutput.IncludesPrefixedGoogleBuilder()
			assertOutput.IncludesPrefixedHerokuBuilders()
			assertOutput.IncludesPrefixedPaketoBuilders()
		})
	})

	when("buildpack", func() {
		when("package", func() {
			var (
				tmpDir                         string
				buildpackManager               buildpacks.BuildModuleManager
				simplePackageConfigFixtureName = "package.toml"
			)

			it.Before(func() {
				var err error
				tmpDir, err = os.MkdirTemp("", "buildpack-package-tests")
				assert.Nil(err)

				buildpackManager = buildpacks.NewBuildModuleManager(t, assert)
				buildpackManager.PrepareBuildModules(tmpDir, buildpacks.BpSimpleLayersParent, buildpacks.BpSimpleLayers)
			})

			it.After(func() {
				assert.Nil(os.RemoveAll(tmpDir))
			})

			generateAggregatePackageToml := func(buildpackURI, nestedPackageName, operatingSystem string) string {
				t.Helper()
				packageTomlFile, err := os.CreateTemp(tmpDir, "package_aggregate-*.toml")
				assert.Nil(err)

				pack.FixtureManager().TemplateFixtureToFile(
					"package_aggregate.toml",
					packageTomlFile,
					map[string]interface{}{
						"BuildpackURI": buildpackURI,
						"PackageName":  nestedPackageName,
						"OS":           operatingSystem,
					},
				)

				assert.Nil(packageTomlFile.Close())

				return packageTomlFile.Name()
			}

			when("no --format is provided", func() {
				it("creates the package as image", func() {
					packageName := "test/package-" + h.RandString(10)
					packageTomlPath := generatePackageTomlWithOS(t, assert, pack, tmpDir, simplePackageConfigFixtureName, imageManager.HostOS())

					output := pack.RunSuccessfully("buildpack", "package", packageName, "-c", packageTomlPath)
					assertions.NewOutputAssertionManager(t, output).ReportsPackageCreation(packageName)
					defer imageManager.CleanupImages(packageName)

					assertImage.ExistsLocally(packageName)
				})
			})

			when("--format image", func() {
				it("creates the package", func() {
					t.Log("package w/ only buildpacks")
					nestedPackageName := "test/package-" + h.RandString(10)
					packageName := "test/package-" + h.RandString(10)

					packageTomlPath := generatePackageTomlWithOS(t, assert, pack, tmpDir, simplePackageConfigFixtureName, imageManager.HostOS())
					aggregatePackageToml := generateAggregatePackageToml("simple-layers-parent-buildpack.tgz", nestedPackageName, imageManager.HostOS())

					packageBuildpack := buildpacks.NewPackageImage(
						t,
						pack,
						packageName,
						aggregatePackageToml,
						buildpacks.WithRequiredBuildpacks(
							buildpacks.BpSimpleLayersParent,
							buildpacks.NewPackageImage(
								t,
								pack,
								nestedPackageName,
								packageTomlPath,
								buildpacks.WithRequiredBuildpacks(buildpacks.BpSimpleLayers),
							),
						),
					)
					buildpackManager.PrepareBuildModules(tmpDir, packageBuildpack)
					defer imageManager.CleanupImages(nestedPackageName, packageName)

					assertImage.ExistsLocally(nestedPackageName)
					assertImage.ExistsLocally(packageName)
				})

				when("--publish", func() {
					it("publishes image to registry", func() {
						packageTomlPath := generatePackageTomlWithOS(t, assert, pack, tmpDir, simplePackageConfigFixtureName, imageManager.HostOS())
						nestedPackageName := registryConfig.RepoName("test/package-" + h.RandString(10))

						nestedPackage := buildpacks.NewPackageImage(
							t,
							pack,
							nestedPackageName,
							packageTomlPath,
							buildpacks.WithRequiredBuildpacks(buildpacks.BpSimpleLayers),
							buildpacks.WithPublish(),
						)
						buildpackManager.PrepareBuildModules(tmpDir, nestedPackage)

						aggregatePackageToml := generateAggregatePackageToml("simple-layers-parent-buildpack.tgz", nestedPackageName, imageManager.HostOS())
						packageName := registryConfig.RepoName("test/package-" + h.RandString(10))

						output := pack.RunSuccessfully(
							"buildpack", "package", packageName,
							"-c", aggregatePackageToml,
							"--publish",
						)

						defer imageManager.CleanupImages(packageName)
						assertions.NewOutputAssertionManager(t, output).ReportsPackagePublished(packageName)

						assertImage.NotExistsLocally(packageName)
						assertImage.CanBePulledFromRegistry(packageName)
					})
				})

				when("--pull-policy=never", func() {
					it("should use local image", func() {
						packageTomlPath := generatePackageTomlWithOS(t, assert, pack, tmpDir, simplePackageConfigFixtureName, imageManager.HostOS())
						nestedPackageName := "test/package-" + h.RandString(10)
						nestedPackage := buildpacks.NewPackageImage(
							t,
							pack,
							nestedPackageName,
							packageTomlPath,
							buildpacks.WithRequiredBuildpacks(buildpacks.BpSimpleLayers),
						)
						buildpackManager.PrepareBuildModules(tmpDir, nestedPackage)
						defer imageManager.CleanupImages(nestedPackageName)
						aggregatePackageToml := generateAggregatePackageToml("simple-layers-parent-buildpack.tgz", nestedPackageName, imageManager.HostOS())

						packageName := registryConfig.RepoName("test/package-" + h.RandString(10))
						defer imageManager.CleanupImages(packageName)
						pack.JustRunSuccessfully(
							"buildpack", "package", packageName,
							"-c", aggregatePackageToml,
							"--pull-policy", "never")

						assertImage.ExistsLocally(packageName)
					})

					it("should not pull image from registry", func() {
						packageTomlPath := generatePackageTomlWithOS(t, assert, pack, tmpDir, simplePackageConfigFixtureName, imageManager.HostOS())
						nestedPackageName := registryConfig.RepoName("test/package-" + h.RandString(10))
						nestedPackage := buildpacks.NewPackageImage(
							t,
							pack,
							nestedPackageName,
							packageTomlPath,
							buildpacks.WithPublish(),
							buildpacks.WithRequiredBuildpacks(buildpacks.BpSimpleLayers),
						)
						buildpackManager.PrepareBuildModules(tmpDir, nestedPackage)
						aggregatePackageToml := generateAggregatePackageToml("simple-layers-parent-buildpack.tgz", nestedPackageName, imageManager.HostOS())

						packageName := registryConfig.RepoName("test/package-" + h.RandString(10))

						output, err := pack.Run(
							"buildpack", "package", packageName,
							"-c", aggregatePackageToml,
							"--pull-policy", "never",
						)

						assert.NotNil(err)
						assertions.NewOutputAssertionManager(t, output).ReportsImageNotExistingOnDaemon(nestedPackageName)
					})
				})
			})

			when("--format file", func() {
				when("the file extension is .cnb", func() {
					it("creates the package with the same extension", func() {
						packageTomlPath := generatePackageTomlWithOS(t, assert, pack, tmpDir, simplePackageConfigFixtureName, imageManager.HostOS())
						destinationFile := filepath.Join(tmpDir, "package.cnb")
						output := pack.RunSuccessfully(
							"buildpack", "package", destinationFile,
							"--format", "file",
							"-c", packageTomlPath,
						)
						assertions.NewOutputAssertionManager(t, output).ReportsPackageCreation(destinationFile)
						h.AssertTarball(t, destinationFile)
					})
				})
				when("the file extension is empty", func() {
					it("creates the package with a .cnb extension", func() {
						packageTomlPath := generatePackageTomlWithOS(t, assert, pack, tmpDir, simplePackageConfigFixtureName, imageManager.HostOS())
						destinationFile := filepath.Join(tmpDir, "package")
						expectedFile := filepath.Join(tmpDir, "package.cnb")
						output := pack.RunSuccessfully(
							"buildpack", "package", destinationFile,
							"--format", "file",
							"-c", packageTomlPath,
						)
						assertions.NewOutputAssertionManager(t, output).ReportsPackageCreation(expectedFile)
						h.AssertTarball(t, expectedFile)
					})
				})
				when("the file extension is not .cnb", func() {
					it("creates the package with the given extension but shows a warning", func() {
						packageTomlPath := generatePackageTomlWithOS(t, assert, pack, tmpDir, simplePackageConfigFixtureName, imageManager.HostOS())
						destinationFile := filepath.Join(tmpDir, "package.tar.gz")
						output := pack.RunSuccessfully(
							"buildpack", "package", destinationFile,
							"--format", "file",
							"-c", packageTomlPath,
						)
						assertOutput := assertions.NewOutputAssertionManager(t, output)
						assertOutput.ReportsPackageCreation(destinationFile)
						assertOutput.ReportsInvalidExtension(".gz")
						h.AssertTarball(t, destinationFile)
					})
				})
			})

			when("package.toml is invalid", func() {
				it("displays an error", func() {
					output, err := pack.Run(
						"buildpack", "package", "some-package",
						"-c", pack.FixtureManager().FixtureLocation("invalid_package.toml"),
					)

					assert.NotNil(err)
					assertOutput := assertions.NewOutputAssertionManager(t, output)
					assertOutput.ReportsReadingConfig()
				})
			})
		})

		when("inspect", func() {
			var tmpDir string

			it.Before(func() {
				var err error
				tmpDir, err = os.MkdirTemp("", "buildpack-inspect-tests")
				assert.Nil(err)
			})

			it.After(func() {
				assert.Succeeds(os.RemoveAll(tmpDir))
			})

			when("buildpack archive", func() {
				it("succeeds", func() {

					packageFileLocation := filepath.Join(
						tmpDir,
						fmt.Sprintf("buildpack-%s.cnb", h.RandString(8)),
					)

					packageTomlPath := generatePackageTomlWithOS(t, assert, pack, tmpDir, "package_for_build_cmd.toml", imageManager.HostOS())

					packageFile := buildpacks.NewPackageFile(
						t,
						pack,
						packageFileLocation,
						packageTomlPath,
						buildpacks.WithRequiredBuildpacks(
							buildpacks.BpFolderSimpleLayersParent,
							buildpacks.BpFolderSimpleLayers,
						),
					)

					buildpackManager.PrepareBuildModules(tmpDir, packageFile)

					expectedOutput := pack.FixtureManager().TemplateFixture(
						"inspect_buildpack_output.txt",
						map[string]interface{}{
							"buildpack_source": "LOCAL ARCHIVE",
							"buildpack_name":   packageFileLocation,
						},
					)

					output := pack.RunSuccessfully("buildpack", "inspect", packageFileLocation)
					assert.TrimmedEq(output, expectedOutput)
				})
			})

			when("buildpack image", func() {
				when("inspect", func() {
					it("succeeds", func() {
						packageTomlPath := generatePackageTomlWithOS(t, assert, pack, tmpDir, "package_for_build_cmd.toml", imageManager.HostOS())
						packageImageName := registryConfig.RepoName("buildpack-" + h.RandString(8))

						packageImage := buildpacks.NewPackageImage(
							t,
							pack,
							packageImageName,
							packageTomlPath,
							buildpacks.WithRequiredBuildpacks(
								buildpacks.BpFolderSimpleLayersParent,
								buildpacks.BpFolderSimpleLayers,
							),
						)
						defer imageManager.CleanupImages(packageImageName)

						buildpackManager.PrepareBuildModules(tmpDir, packageImage)

						expectedOutput := pack.FixtureManager().TemplateFixture(
							"inspect_buildpack_output.txt",
							map[string]interface{}{
								"buildpack_source": "LOCAL IMAGE",
								"buildpack_name":   packageImageName,
							},
						)

						output := pack.RunSuccessfully("buildpack", "inspect", packageImageName)
						assert.TrimmedEq(output, expectedOutput)
					})
				})
			})
		})
	})

	when("builder", func() {
		when("suggest", func() {
			it("displays suggested builders", func() {
				output := pack.RunSuccessfully("builder", "suggest")

				assertOutput := assertions.NewOutputAssertionManager(t, output)
				assertOutput.IncludesSuggestedBuildersHeading()
				assertOutput.IncludesPrefixedGoogleBuilder()
				assertOutput.IncludesPrefixedHerokuBuilders()
				assertOutput.IncludesPrefixedPaketoBuilders()
			})
		})
	})

	when("config", func() {
		when("default-builder", func() {
			it("sets the default builder in ~/.pack/config.toml", func() {
				builderName := "paketobuildpacks/builder-jammy-base"
				output := pack.RunSuccessfully("config", "default-builder", builderName)

				assertions.NewOutputAssertionManager(t, output).ReportsSettingDefaultBuilder(builderName)
			})
		})

		when("trusted-builders", func() {
			it("prints list of trusted builders", func() {
				output := pack.RunSuccessfully("config", "trusted-builders")

				assertOutput := assertions.NewOutputAssertionManager(t, output)
				assertOutput.IncludesTrustedBuildersHeading()
				assertOutput.IncludesHerokuBuilders()
				assertOutput.IncludesGoogleBuilder()
				assertOutput.IncludesPaketoBuilders()
			})

			when("add", func() {
				it("sets the builder as trusted in ~/.pack/config.toml", func() {
					builderName := "some-builder" + h.RandString(10)

					pack.JustRunSuccessfully("config", "trusted-builders", "add", builderName)
					assert.Contains(pack.ConfigFileContents(), builderName)
				})
			})

			when("remove", func() {
				it("removes the previously trusted builder from ~/${PACK_HOME}/config.toml", func() {
					builderName := "some-builder" + h.RandString(10)

					pack.JustRunSuccessfully("config", "trusted-builders", "add", builderName)

					assert.Contains(pack.ConfigFileContents(), builderName)

					pack.JustRunSuccessfully("config", "trusted-builders", "remove", builderName)

					assert.NotContains(pack.ConfigFileContents(), builderName)
				})
			})

			when("list", func() {
				it("prints list of trusted builders", func() {
					output := pack.RunSuccessfully("config", "trusted-builders", "list")

					assertOutput := assertions.NewOutputAssertionManager(t, output)
					assertOutput.IncludesTrustedBuildersHeading()
					assertOutput.IncludesHerokuBuilders()
					assertOutput.IncludesGoogleBuilder()
					assertOutput.IncludesPaketoBuilders()
				})

				it("shows a builder trusted by pack config trusted-builders add", func() {
					builderName := "some-builder" + h.RandString(10)

					pack.JustRunSuccessfully("config", "trusted-builders", "add", builderName)

					output := pack.RunSuccessfully("config", "trusted-builders", "list")
					assert.Contains(output, builderName)
				})
			})
		})
	})

	when("stack", func() {
		when("suggest", func() {
			it("displays suggested stacks", func() {
				output, err := pack.Run("stack", "suggest")
				assert.Nil(err)

				assertions.NewOutputAssertionManager(t, output).IncludesSuggestedStacksHeading()
			})
		})
	})

	when("report", func() {
		when("default builder is set", func() {
			it("redacts default builder", func() {
				pack.RunSuccessfully("config", "default-builder", "paketobuildpacks/builder-jammy-base")

				output := pack.RunSuccessfully("report")
				version := pack.Version()

				layoutRepoDir := filepath.Join(pack.Home(), "layout-repo")
				if runtime.GOOS == "windows" {
					layoutRepoDir = strings.ReplaceAll(layoutRepoDir, `\`, `\\`)
				}

				expectedOutput := pack.FixtureManager().TemplateFixture(
					"report_output.txt",
					map[string]interface{}{
						"DefaultBuilder": "[REDACTED]",
						"Version":        version,
						"OS":             runtime.GOOS,
						"Arch":           runtime.GOARCH,
						"LayoutRepoDir":  layoutRepoDir,
					},
				)
				assert.Equal(output, expectedOutput)
			})

			it("explicit mode doesn't redact", func() {
				pack.RunSuccessfully("config", "default-builder", "paketobuildpacks/builder-jammy-base")

				output := pack.RunSuccessfully("report", "--explicit")
				version := pack.Version()

				layoutRepoDir := filepath.Join(pack.Home(), "layout-repo")
				if runtime.GOOS == "windows" {
					layoutRepoDir = strings.ReplaceAll(layoutRepoDir, `\`, `\\`)
				}

				expectedOutput := pack.FixtureManager().TemplateFixture(
					"report_output.txt",
					map[string]interface{}{
						"DefaultBuilder": "paketobuildpacks/builder-jammy-base",
						"Version":        version,
						"OS":             runtime.GOOS,
						"Arch":           runtime.GOARCH,
						"LayoutRepoDir":  layoutRepoDir,
					},
				)
				assert.Equal(output, expectedOutput)
			})
		})
	})
}

func testAcceptance(
	t *testing.T,
	when spec.G,
	it spec.S,
	subjectPackConfig, createBuilderPackConfig config.PackAsset,
	lifecycle config.LifecycleAsset,
) {
	var (
		pack, createBuilderPack *invoke.PackInvoker
		buildpackManager        buildpacks.BuildModuleManager
		bpDir                   = buildModulesDir(lifecycle.EarliestBuildpackAPIVersion())
		assert                  = h.NewAssertionManager(t)
	)

	it.Before(func() {
		pack = invoke.NewPackInvoker(t, assert, subjectPackConfig, registryConfig.DockerConfigDir)
		pack.EnableExperimental()

		createBuilderPack = invoke.NewPackInvoker(t, assert, createBuilderPackConfig, registryConfig.DockerConfigDir)
		createBuilderPack.EnableExperimental()

		buildpackManager = buildpacks.NewBuildModuleManager(
			t,
			assert,
			buildpacks.WithBuildpackAPIVersion(lifecycle.EarliestBuildpackAPIVersion()),
		)
	})

	it.After(func() {
		pack.Cleanup()
		createBuilderPack.Cleanup()
	})

	when("stack is created", func() {
		var (
			runImageMirror  string
			stackBaseImages = map[string][]string{
				"linux":   {"ubuntu:bionic"},
				"windows": {"mcr.microsoft.com/windows/nanoserver:1809", "golang:1.17-nanoserver-1809"},
			}
		)

		it.Before(func() {
			value, err := suiteManager.RunTaskOnceString("create-stack",
				func() (string, error) {
					runImageMirror := registryConfig.RepoName(runImage)
					err := createStack(t, dockerCli, runImageMirror)
					if err != nil {
						return "", err
					}

					return runImageMirror, nil
				})
			assert.Nil(err)

			baseStackNames := stackBaseImages[imageManager.HostOS()]
			suiteManager.RegisterCleanUp("remove-stack-images", func() error {
				imageManager.CleanupImages(baseStackNames...)
				imageManager.CleanupImages(runImage, buildImage, value)
				return nil
			})

			runImageMirror = value
		})

		when("builder is created", func() {
			var builderName string

			it.Before(func() {
				key := taskKey(
					"create-builder",
					append(
						[]string{runImageMirror, createBuilderPackConfig.Path(), lifecycle.Identifier()},
						createBuilderPackConfig.FixturePaths()...,
					)...,
				)
				value, err := suiteManager.RunTaskOnceString(key, func() (string, error) {
					return createBuilder(t, assert, createBuilderPack, lifecycle, buildpackManager, runImageMirror)
				})
				assert.Nil(err)
				suiteManager.RegisterCleanUp("clean-"+key, func() error {
					imageManager.CleanupImages(value)
					return nil
				})

				builderName = value
			})

			when("complex builder", func() {
				when("builder has duplicate buildpacks", func() {
					it.Before(func() {
						// create our nested builder
						h.SkipIf(t, imageManager.HostOS() == "windows", "These tests are not yet compatible with Windows-based containers")

						// create a task, handled by a 'task manager' which executes our pack commands during tests.
						// looks like this is used to de-dup tasks
						key := taskKey(
							"create-complex-builder",
							append(
								[]string{runImageMirror, createBuilderPackConfig.Path(), lifecycle.Identifier()},
								createBuilderPackConfig.FixturePaths()...,
							)...,
						)

						value, err := suiteManager.RunTaskOnceString(key, func() (string, error) {
							return createComplexBuilder(
								t,
								assert,
								createBuilderPack,
								lifecycle,
								buildpackManager,
								runImageMirror,
							)
						})
						assert.Nil(err)

						// register task to be run to 'clean up' a task
						suiteManager.RegisterCleanUp("clean-"+key, func() error {
							imageManager.CleanupImages(value)
							return nil
						})
						builderName = value

						output := pack.RunSuccessfully(
							"config", "run-image-mirrors", "add", "pack-test/run", "--mirror", "some-registry.com/pack-test/run1")
						assertOutput := assertions.NewOutputAssertionManager(t, output)
						assertOutput.ReportsSuccesfulRunImageMirrorsAdd("pack-test/run", "some-registry.com/pack-test/run1")
					})

					it("buildpack layers have no duplication", func() {
						assertImage.DoesNotHaveDuplicateLayers(builderName)
					})
				})

				when("builder has extensions", func() {
					it.Before(func() {
						h.SkipIf(t, !createBuilderPack.SupportsFeature(invoke.BuildImageExtensions), "")
						h.SkipIf(t, !pack.SupportsFeature(invoke.BuildImageExtensions), "")
						h.SkipIf(t, !lifecycle.SupportsFeature(config.BuildImageExtensions), "")
						// create a task, handled by a 'task manager' which executes our pack commands during tests.
						// looks like this is used to de-dup tasks
						key := taskKey(
							"create-builder-with-extensions",
							append(
								[]string{runImageMirror, createBuilderPackConfig.Path(), lifecycle.Identifier()},
								createBuilderPackConfig.FixturePaths()...,
							)...,
						)

						value, err := suiteManager.RunTaskOnceString(key, func() (string, error) {
							return createBuilderWithExtensions(
								t,
								assert,
								createBuilderPack,
								lifecycle,
								buildpackManager,
								runImageMirror,
							)
						})
						assert.Nil(err)

						// register task to be run to 'clean up' a task
						suiteManager.RegisterCleanUp("clean-"+key, func() error {
							imageManager.CleanupImages(value)
							return nil
						})
						builderName = value
					})

					it("creates builder", func() {
						// Linux containers (including Linux containers on Windows)
						extSimpleLayersDiffID := "sha256:b9e4a0ddfb650c7aa71d1e6aceea1665365e409b3078bfdc1e51c2b07ab2b423"
						extReadEnvDiffID := "sha256:ab7419c5e0b1a0789bd07cef2ed0573ec6e98eb05d7f05eb95d4f035243e331c"
						bpSimpleLayersDiffID := "sha256:285ff6683c99e5ae19805f6a62168fb40dca64d813c53b782604c9652d745c70"
						bpReadEnvDiffID := "sha256:dd1e0efcbf3f08b014ef6eff9cfe7a9eac1cf20bd9b6a71a946f0a74575aa56f"
						if imageManager.HostOS() == "windows" { // Windows containers on Windows
							extSimpleLayersDiffID = "sha256:a063cf949b9c267133e451ac8cd95b4e77571bb7c629dd817461dca769170810"
							extReadEnvDiffID = "sha256:a4e7f114efa3692939974da9c9f08e47b3fdb5c779688dc8f5a950e0f804bef1"
							bpSimpleLayersDiffID = "sha256:ccd1234cc5685e8a412b70c5f9a8e7b584b8e4f2a20c987ec242c9055de3e45e"
							bpReadEnvDiffID = "sha256:8b22a7742ffdfbdd978787c6937456b68afb27c3585a3903048be7434d251e3f"
						}
						// extensions
						assertImage.HasLabelWithData(builderName, "io.buildpacks.extension.layers", `{"read/env":{"read-env-version":{"api":"0.9","layerDiffID":"`+extReadEnvDiffID+`","name":"Read Env Extension"}},"simple/layers":{"simple-layers-version":{"api":"0.2","layerDiffID":"`+extSimpleLayersDiffID+`","name":"Simple Layers Extension"}}}`)
						assertImage.HasLabelWithData(builderName, "io.buildpacks.buildpack.order-extensions", `[{"group":[{"id":"read/env","version":"read-env-version"},{"id":"simple/layers","version":"simple-layers-version"}]}]`)
						// buildpacks
						assertImage.HasLabelWithData(builderName, "io.buildpacks.buildpack.layers", `{"read/env":{"read-env-version":{"api":"0.2","stacks":[{"id":"pack.test.stack"}],"layerDiffID":"`+bpReadEnvDiffID+`","name":"Read Env Buildpack"}},"simple/layers":{"simple-layers-version":{"api":"0.2","stacks":[{"id":"pack.test.stack"}],"layerDiffID":"`+bpSimpleLayersDiffID+`","name":"Simple Layers Buildpack"}}}`)
						assertImage.HasLabelWithData(builderName, "io.buildpacks.buildpack.order", `[{"group":[{"id":"read/env","version":"read-env-version","optional":true},{"id":"simple/layers","version":"simple-layers-version","optional":true}]}]`)
					})

					when("build", func() {
						var repo, repoName string

						it.Before(func() {
							h.SkipIf(t, imageManager.HostOS() == "windows", "")

							repo = "some-org/" + h.RandString(10)
							repoName = registryConfig.RepoName(repo)
							pack.JustRunSuccessfully("config", "lifecycle-image", lifecycle.Image())
						})

						it.After(func() {
							h.SkipIf(t, imageManager.HostOS() == "windows", "")

							imageManager.CleanupImages(repoName)
							ref, err := name.ParseReference(repoName, name.WeakValidation)
							assert.Nil(err)
							cacheImage := cache.NewImageCache(ref, dockerCli)
							buildCacheVolume := cache.NewVolumeCache(ref, cache.CacheInfo{}, "build", dockerCli)
							launchCacheVolume := cache.NewVolumeCache(ref, cache.CacheInfo{}, "launch", dockerCli)
							cacheImage.Clear(context.TODO())
							buildCacheVolume.Clear(context.TODO())
							launchCacheVolume.Clear(context.TODO())
						})

						when("builder is untrusted", func() {
							it("uses the 5 phases, and runs the extender (build)", func() {
								output := pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--network", "host", // export target is the daemon, but we need to be able to reach the registry where the builder image is saved
									"-B", builderName,
								)

								assertions.NewOutputAssertionManager(t, output).ReportsSuccessfulImageBuild(repoName)

								assertOutput := assertions.NewLifecycleOutputAssertionManager(t, output)
								assertOutput.IncludesLifecycleImageTag(lifecycle.Image())
								assertOutput.IncludesSeparatePhasesWithBuildExtension()

								t.Log("inspecting image")
								inspectCmd := "inspect"
								if !pack.Supports("inspect") {
									inspectCmd = "inspect-image"
								}

								output = pack.RunSuccessfully(inspectCmd, repoName)
							})
						})

						when("there are run image extensions", func() {
							it.Before(func() {
								h.SkipIf(t, !pack.SupportsFeature(invoke.RunImageExtensions), "")
								h.SkipIf(t, !lifecycle.SupportsFeature(config.RunImageExtensions), "")
							})

							it("uses the 5 phases, and runs the extender (run)", func() {
								output := pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--network", "host", // export target is the daemon, but we need to be able to reach the registry where the builder image and run image are saved
									"-B", builderName,
									"--env", "EXT_RUN=1",
								)

								assertions.NewOutputAssertionManager(t, output).ReportsSuccessfulImageBuild(repoName)

								assertOutput := assertions.NewLifecycleOutputAssertionManager(t, output)
								assertOutput.IncludesLifecycleImageTag(lifecycle.Image())
								assertOutput.IncludesSeparatePhasesWithRunExtension()

								t.Log("inspecting image")
								inspectCmd := "inspect"
								if !pack.Supports("inspect") {
									inspectCmd = "inspect-image"
								}

								output = pack.RunSuccessfully(inspectCmd, repoName)
							})
						})
					})
				})
			})

			when("builder.toml is invalid", func() {
				it("displays an error", func() {
					builderConfigPath := createBuilderPack.FixtureManager().FixtureLocation("invalid_builder.toml")

					output, err := createBuilderPack.Run(
						"builder", "create", "some-builder:build",
						"--config", builderConfigPath,
					)

					assert.NotNil(err)
					assertOutput := assertions.NewOutputAssertionManager(t, output)
					assertOutput.ReportsInvalidBuilderToml()
				})
			})

			when("build", func() {
				var repo, repoName string

				it.Before(func() {
					repo = "some-org/" + h.RandString(10)
					repoName = registryConfig.RepoName(repo)
					pack.JustRunSuccessfully("config", "lifecycle-image", lifecycle.Image())
				})

				it.After(func() {
					imageManager.CleanupImages(repoName)
					ref, err := name.ParseReference(repoName, name.WeakValidation)
					assert.Nil(err)
					cacheImage := cache.NewImageCache(ref, dockerCli)
					buildCacheVolume := cache.NewVolumeCache(ref, cache.CacheInfo{}, "build", dockerCli)
					launchCacheVolume := cache.NewVolumeCache(ref, cache.CacheInfo{}, "launch", dockerCli)
					cacheImage.Clear(context.TODO())
					buildCacheVolume.Clear(context.TODO())
					launchCacheVolume.Clear(context.TODO())
				})

				when("builder is untrusted", func() {
					var untrustedBuilderName string
					it.Before(func() {
						var err error
						untrustedBuilderName, err = createBuilder(
							t,
							assert,
							createBuilderPack,
							lifecycle,
							buildpackManager,
							runImageMirror,
						)
						assert.Nil(err)

						suiteManager.RegisterCleanUp("remove-lifecycle-"+lifecycle.Image(), func() error {
							img := imageManager.GetImageID(lifecycle.Image())
							imageManager.CleanupImages(img)
							return nil
						})
					})

					it.After(func() {
						imageManager.CleanupImages(untrustedBuilderName)
					})

					when("daemon", func() {
						it("uses the 5 phases", func() {
							output := pack.RunSuccessfully(
								"build", repoName,
								"-p", filepath.Join("testdata", "mock_app"),
								"-B", untrustedBuilderName,
							)

							assertions.NewOutputAssertionManager(t, output).ReportsSuccessfulImageBuild(repoName)

							assertOutput := assertions.NewLifecycleOutputAssertionManager(t, output)
							assertOutput.IncludesLifecycleImageTag(lifecycle.Image())
							assertOutput.IncludesSeparatePhases()
						})
					})

					when("--publish", func() {
						it("uses the 5 phases", func() {
							buildArgs := []string{
								repoName,
								"-p", filepath.Join("testdata", "mock_app"),
								"-B", untrustedBuilderName,
								"--publish",
							}
							if imageManager.HostOS() != "windows" {
								buildArgs = append(buildArgs, "--network", "host")
							}

							output := pack.RunSuccessfully("build", buildArgs...)

							assertions.NewOutputAssertionManager(t, output).ReportsSuccessfulImageBuild(repoName)

							assertOutput := assertions.NewLifecycleOutputAssertionManager(t, output)
							assertOutput.IncludesLifecycleImageTag(lifecycle.Image())
							assertOutput.IncludesSeparatePhases()
						})
					})

					when("additional tags", func() {
						var additionalRepoName string

						it.Before(func() {
							additionalRepoName = fmt.Sprintf("%s_additional", repoName)
						})
						it.After(func() {
							imageManager.CleanupImages(additionalRepoName)
						})
						it("pushes image to additional tags", func() {
							output := pack.RunSuccessfully(
								"build", repoName,
								"-p", filepath.Join("testdata", "mock_app"),
								"-B", untrustedBuilderName,
								"--tag", additionalRepoName,
							)

							assertions.NewOutputAssertionManager(t, output).ReportsSuccessfulImageBuild(repoName)
							assert.Contains(output, additionalRepoName)
						})
					})
				})

				when("builder is trusted (and set as default)", func() {
					it.Before(func() {
						pack.RunSuccessfully("config", "default-builder", builderName)
						pack.JustRunSuccessfully("config", "trusted-builders", "add", builderName)
					})

					it("creates a runnable, rebuildable image on daemon from app dir", func() {
						appPath := filepath.Join("testdata", "mock_app")

						output := pack.RunSuccessfully(
							"build", repoName,
							"-p", appPath,
						)

						assertOutput := assertions.NewOutputAssertionManager(t, output)

						assertOutput.ReportsSuccessfulImageBuild(repoName)
						assertOutput.ReportsUsingBuildCacheVolume()
						assertOutput.ReportsSelectingRunImageMirror(runImageMirror)

						t.Log("app is runnable")
						assertImage.RunsWithOutput(repoName, "Launch Dep Contents", "Cached Dep Contents")

						t.Log("it uses the run image as a base image")
						assertImage.HasBaseImage(repoName, runImage)

						t.Log("sets the run image metadata")
						assertImage.HasLabelWithData(repoName, "io.buildpacks.lifecycle.metadata", fmt.Sprintf(`"image":"pack-test/run","mirrors":["%s"]`, runImageMirror))

						t.Log("sets the source metadata")
						assertImage.HasLabelWithData(repoName, "io.buildpacks.project.metadata", (`{"source":{"type":"project","version":{"declared":"1.0.2"},"metadata":{"url":"https://github.com/buildpacks/pack"}}}`))

						t.Log("registry is empty")
						assertImage.NotExistsInRegistry(repo)

						t.Log("add a local mirror")
						localRunImageMirror := registryConfig.RepoName("pack-test/run-mirror")
						imageManager.TagImage(runImage, localRunImageMirror)
						defer imageManager.CleanupImages(localRunImageMirror)
						pack.JustRunSuccessfully("config", "run-image-mirrors", "add", runImage, "-m", localRunImageMirror)

						t.Log("rebuild")
						output = pack.RunSuccessfully(
							"build", repoName,
							"-p", appPath,
						)
						assertOutput = assertions.NewOutputAssertionManager(t, output)
						assertOutput.ReportsSuccessfulImageBuild(repoName)
						assertOutput.ReportsSelectingRunImageMirrorFromLocalConfig(localRunImageMirror)
						cachedLaunchLayer := "simple/layers:cached-launch-layer"

						assertLifecycleOutput := assertions.NewLifecycleOutputAssertionManager(t, output)
						assertLifecycleOutput.ReportsRestoresCachedLayer(cachedLaunchLayer)
						assertLifecycleOutput.ReportsExporterReusingUnchangedLayer(cachedLaunchLayer)
						assertLifecycleOutput.ReportsCacheReuse(cachedLaunchLayer)

						t.Log("app is runnable")
						assertImage.RunsWithOutput(repoName, "Launch Dep Contents", "Cached Dep Contents")

						t.Log("rebuild with --clear-cache")
						output = pack.RunSuccessfully("build", repoName, "-p", appPath, "--clear-cache")

						assertOutput = assertions.NewOutputAssertionManager(t, output)
						assertOutput.ReportsSuccessfulImageBuild(repoName)
						assertLifecycleOutput = assertions.NewLifecycleOutputAssertionManager(t, output)
						assertLifecycleOutput.ReportsExporterReusingUnchangedLayer(cachedLaunchLayer)
						assertLifecycleOutput.ReportsCacheCreation(cachedLaunchLayer)

						t.Log("cacher adds layers")
						assert.Matches(output, regexp.MustCompile(`(?i)Adding cache layer 'simple/layers:cached-launch-layer'`))

						t.Log("inspecting image")
						inspectCmd := "inspect"
						if !pack.Supports("inspect") {
							inspectCmd = "inspect-image"
						}

						var (
							webCommand      string
							helloCommand    string
							helloArgs       []string
							helloArgsPrefix string
							imageWorkdir    string
						)
						if imageManager.HostOS() == "windows" {
							webCommand = ".\\run"
							helloCommand = "cmd"
							helloArgs = []string{"/c", "echo hello world"}
							helloArgsPrefix = " "
							imageWorkdir = "c:\\workspace"

						} else {
							webCommand = "./run"
							helloCommand = "echo"
							helloArgs = []string{"hello", "world"}
							helloArgsPrefix = ""
							imageWorkdir = "/workspace"
						}

						formats := []compareFormat{
							{
								extension:   "json",
								compareFunc: assert.EqualJSON,
								outputArg:   "json",
							},
							{
								extension:   "yaml",
								compareFunc: assert.EqualYAML,
								outputArg:   "yaml",
							},
							{
								extension:   "toml",
								compareFunc: assert.EqualTOML,
								outputArg:   "toml",
							},
						}
						for _, format := range formats {
							t.Logf("inspecting image %s format", format.outputArg)

							output = pack.RunSuccessfully(inspectCmd, repoName, "--output", format.outputArg)
							expectedOutput := pack.FixtureManager().TemplateFixture(
								fmt.Sprintf("inspect_image_local_output.%s", format.extension),
								map[string]interface{}{
									"image_name":             repoName,
									"base_image_id":          h.ImageID(t, runImageMirror),
									"base_image_top_layer":   h.TopLayerDiffID(t, runImageMirror),
									"run_image_local_mirror": localRunImageMirror,
									"run_image_mirror":       runImageMirror,
									"web_command":            webCommand,
									"hello_command":          helloCommand,
									"hello_args":             helloArgs,
									"hello_args_prefix":      helloArgsPrefix,
									"image_workdir":          imageWorkdir,
								},
							)

							format.compareFunc(output, expectedOutput)
						}
					})

					when("--no-color", func() {
						it("doesn't have color", func() {
							appPath := filepath.Join("testdata", "mock_app")

							// --no-color is set as a default option in our tests, and doesn't need to be explicitly provided
							output := pack.RunSuccessfully("build", repoName, "-p", appPath)
							assertOutput := assertions.NewOutputAssertionManager(t, output)
							assertOutput.ReportsSuccessfulImageBuild(repoName)
							assertOutput.WithoutColors()
						})
					})

					when("--quiet", func() {
						it("only logs app name and sha", func() {
							appPath := filepath.Join("testdata", "mock_app")

							pack.SetVerbose(false)
							defer pack.SetVerbose(true)

							output := pack.RunSuccessfully("build", repoName, "-p", appPath, "--quiet")
							assertOutput := assertions.NewOutputAssertionManager(t, output)
							assertOutput.ReportSuccessfulQuietBuild(repoName)
						})
					})

					it("supports building app from a zip file", func() {
						appPath := filepath.Join("testdata", "mock_app.zip")
						output := pack.RunSuccessfully("build", repoName, "-p", appPath)
						assertions.NewOutputAssertionManager(t, output).ReportsSuccessfulImageBuild(repoName)
					})

					when("--network", func() {
						var tmpDir string

						it.Before(func() {
							h.SkipIf(t, imageManager.HostOS() == "windows", "temporarily disabled on WCOW due to CI flakiness")

							var err error
							tmpDir, err = os.MkdirTemp("", "archive-buildpacks-")
							assert.Nil(err)

							buildpackManager.PrepareBuildModules(tmpDir, buildpacks.BpInternetCapable)
						})

						it.After(func() {
							h.SkipIf(t, imageManager.HostOS() == "windows", "temporarily disabled on WCOW due to CI flakiness")
							assert.Succeeds(os.RemoveAll(tmpDir))
						})

						when("the network mode is not provided", func() {
							it("reports buildpack access to internet", func() {
								output := pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--buildpack", buildpacks.BpInternetCapable.FullPathIn(tmpDir),
								)

								assertBuildpackOutput := assertions.NewTestBuildpackOutputAssertionManager(t, output)
								assertBuildpackOutput.ReportsConnectedToInternet()
							})
						})

						when("the network mode is set to default", func() {
							it("reports buildpack access to internet", func() {
								output := pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--buildpack", buildpacks.BpInternetCapable.FullPathIn(tmpDir),
									"--network", "default",
								)

								assertBuildpackOutput := assertions.NewTestBuildpackOutputAssertionManager(t, output)
								assertBuildpackOutput.ReportsConnectedToInternet()
							})
						})

						when("the network mode is set to none", func() {
							it("reports buildpack disconnected from internet", func() {
								output := pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--buildpack", buildpacks.BpInternetCapable.FullPathIn(tmpDir),
									"--network", "none",
								)

								assertBuildpackOutput := assertions.NewTestBuildpackOutputAssertionManager(t, output)
								assertBuildpackOutput.ReportsDisconnectedFromInternet()
							})
						})
					})

					when("--volume", func() {
						var (
							volumeRoot   = "/"
							slash        = "/"
							tmpDir       string
							tmpVolumeSrc string
						)

						it.Before(func() {
							h.SkipIf(t, os.Getenv("DOCKER_HOST") != "", "cannot mount volume when DOCKER_HOST is set")

							if imageManager.HostOS() == "windows" {
								volumeRoot = `c:\`
								slash = `\`
							}

							var err error
							tmpDir, err = os.MkdirTemp("", "volume-buildpack-tests-")
							assert.Nil(err)

							buildpackManager.PrepareBuildModules(tmpDir, buildpacks.BpReadVolume, buildpacks.BpReadWriteVolume)

							tmpVolumeSrc, err = os.MkdirTemp("", "volume-mount-source")
							assert.Nil(err)
							assert.Succeeds(os.Chmod(tmpVolumeSrc, 0777)) // Override umask

							// Some OSes (like macOS) use symlinks for the standard temp dir.
							// Resolve it so it can be properly mounted by the Docker daemon.
							tmpVolumeSrc, err = filepath.EvalSymlinks(tmpVolumeSrc)
							assert.Nil(err)

							err = os.WriteFile(filepath.Join(tmpVolumeSrc, "some-file"), []byte("some-content\n"), 0777)
							assert.Nil(err)
						})

						it.After(func() {
							_ = os.RemoveAll(tmpDir)
							_ = os.RemoveAll(tmpVolumeSrc)
						})

						when("volume is read-only", func() {
							it("mounts the provided volume in the detect and build phases", func() {
								volumeDest := volumeRoot + "platform" + slash + "volume-mount-target"
								testFilePath := volumeDest + slash + "some-file"
								output := pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--volume", fmt.Sprintf("%s:%s", tmpVolumeSrc, volumeDest),
									"--buildpack", buildpacks.BpReadVolume.FullPathIn(tmpDir),
									"--env", "TEST_FILE_PATH="+testFilePath,
								)

								bpOutputAsserts := assertions.NewTestBuildpackOutputAssertionManager(t, output)
								bpOutputAsserts.ReportsReadingFileContents("Detect", testFilePath, "some-content")
								bpOutputAsserts.ReportsReadingFileContents("Build", testFilePath, "some-content")
							})

							it("should fail to write", func() {
								volumeDest := volumeRoot + "platform" + slash + "volume-mount-target"
								testDetectFilePath := volumeDest + slash + "detect-file"
								testBuildFilePath := volumeDest + slash + "build-file"
								output := pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--volume", fmt.Sprintf("%s:%s", tmpVolumeSrc, volumeDest),
									"--buildpack", buildpacks.BpReadWriteVolume.FullPathIn(tmpDir),
									"--env", "DETECT_TEST_FILE_PATH="+testDetectFilePath,
									"--env", "BUILD_TEST_FILE_PATH="+testBuildFilePath,
								)

								bpOutputAsserts := assertions.NewTestBuildpackOutputAssertionManager(t, output)
								bpOutputAsserts.ReportsFailingToWriteFileContents("Detect", testDetectFilePath)
								bpOutputAsserts.ReportsFailingToWriteFileContents("Build", testBuildFilePath)
							})
						})

						when("volume is read-write", func() {
							it("can be written to", func() {
								volumeDest := volumeRoot + "volume-mount-target"
								testDetectFilePath := volumeDest + slash + "detect-file"
								testBuildFilePath := volumeDest + slash + "build-file"
								output := pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--volume", fmt.Sprintf("%s:%s:rw", tmpVolumeSrc, volumeDest),
									"--buildpack", buildpacks.BpReadWriteVolume.FullPathIn(tmpDir),
									"--env", "DETECT_TEST_FILE_PATH="+testDetectFilePath,
									"--env", "BUILD_TEST_FILE_PATH="+testBuildFilePath,
								)

								bpOutputAsserts := assertions.NewTestBuildpackOutputAssertionManager(t, output)
								bpOutputAsserts.ReportsWritingFileContents("Detect", testDetectFilePath)
								bpOutputAsserts.ReportsReadingFileContents("Detect", testDetectFilePath, "some-content")
								bpOutputAsserts.ReportsWritingFileContents("Build", testBuildFilePath)
								bpOutputAsserts.ReportsReadingFileContents("Build", testBuildFilePath, "some-content")
							})
						})
					})

					when("--default-process", func() {
						it("sets the default process from those in the process list", func() {
							pack.RunSuccessfully(
								"build", repoName,
								"--default-process", "hello",
								"-p", filepath.Join("testdata", "mock_app"),
							)

							assertImage.RunsWithLogs(repoName, "hello world")
						})
					})

					when("--buildpack", func() {
						when("the argument is an ID", func() {
							it("adds the buildpacks to the builder if necessary and runs them", func() {
								output := pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--buildpack", "simple/layers", // can omit version if only one
									"--buildpack", "noop.buildpack@noop.buildpack.version",
								)

								assertOutput := assertions.NewOutputAssertionManager(t, output)

								assertTestAppOutput := assertions.NewTestBuildpackOutputAssertionManager(t, output)
								assertTestAppOutput.ReportsBuildStep("Simple Layers Buildpack")
								assertTestAppOutput.ReportsBuildStep("NOOP Buildpack")
								assertOutput.ReportsSuccessfulImageBuild(repoName)

								t.Log("app is runnable")
								assertImage.RunsWithOutput(
									repoName,
									"Launch Dep Contents",
									"Cached Dep Contents",
								)
							})
						})

						when("the argument is an archive", func() {
							var tmpDir string

							it.Before(func() {
								var err error
								tmpDir, err = os.MkdirTemp("", "archive-buildpack-tests-")
								assert.Nil(err)
							})

							it.After(func() {
								assert.Succeeds(os.RemoveAll(tmpDir))
							})

							it("adds the buildpack to the builder and runs it", func() {
								buildpackManager.PrepareBuildModules(tmpDir, buildpacks.BpArchiveNotInBuilder)

								output := pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--buildpack", buildpacks.BpArchiveNotInBuilder.FullPathIn(tmpDir),
								)

								assertOutput := assertions.NewOutputAssertionManager(t, output)
								assertOutput.ReportsAddingBuildpack("local/bp", "local-bp-version")
								assertOutput.ReportsSuccessfulImageBuild(repoName)

								assertBuildpackOutput := assertions.NewTestBuildpackOutputAssertionManager(t, output)
								assertBuildpackOutput.ReportsBuildStep("Local Buildpack")
							})
						})

						when("the argument is directory", func() {
							var tmpDir string

							it.Before(func() {
								var err error
								tmpDir, err = os.MkdirTemp("", "folder-buildpack-tests-")
								assert.Nil(err)
							})

							it.After(func() {
								_ = os.RemoveAll(tmpDir)
							})

							it("adds the buildpacks to the builder and runs it", func() {
								h.SkipIf(t, runtime.GOOS == "windows", "buildpack directories not supported on windows")

								buildpackManager.PrepareBuildModules(tmpDir, buildpacks.BpFolderNotInBuilder)

								output := pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--buildpack", buildpacks.BpFolderNotInBuilder.FullPathIn(tmpDir),
								)

								assertOutput := assertions.NewOutputAssertionManager(t, output)
								assertOutput.ReportsAddingBuildpack("local/bp", "local-bp-version")
								assertOutput.ReportsSuccessfulImageBuild(repoName)

								assertBuildpackOutput := assertions.NewTestBuildpackOutputAssertionManager(t, output)
								assertBuildpackOutput.ReportsBuildStep("Local Buildpack")
							})
						})

						when("the argument is a buildpackage image", func() {
							var (
								tmpDir           string
								packageImageName string
							)

							it.After(func() {
								imageManager.CleanupImages(packageImageName)
								_ = os.RemoveAll(tmpDir)
							})

							it("adds the buildpacks to the builder and runs them", func() {
								packageImageName = registryConfig.RepoName("buildpack-" + h.RandString(8))

								packageTomlPath := generatePackageTomlWithOS(t, assert, pack, tmpDir, "package_for_build_cmd.toml", imageManager.HostOS())
								packageImage := buildpacks.NewPackageImage(
									t,
									pack,
									packageImageName,
									packageTomlPath,
									buildpacks.WithRequiredBuildpacks(
										buildpacks.BpFolderSimpleLayersParent,
										buildpacks.BpFolderSimpleLayers,
									),
								)

								buildpackManager.PrepareBuildModules(tmpDir, packageImage)

								output := pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--buildpack", packageImageName,
								)

								assertOutput := assertions.NewOutputAssertionManager(t, output)
								assertOutput.ReportsAddingBuildpack(
									"simple/layers/parent",
									"simple-layers-parent-version",
								)
								assertOutput.ReportsAddingBuildpack("simple/layers", "simple-layers-version")
								assertOutput.ReportsSuccessfulImageBuild(repoName)

								assertBuildpackOutput := assertions.NewTestBuildpackOutputAssertionManager(t, output)
								assertBuildpackOutput.ReportsBuildStep("Simple Layers Buildpack")
							})
						})

						when("the argument is a buildpackage file", func() {
							var tmpDir string

							it.Before(func() {
								var err error
								tmpDir, err = os.MkdirTemp("", "package-file")
								assert.Nil(err)
							})

							it.After(func() {
								assert.Succeeds(os.RemoveAll(tmpDir))
							})

							it("adds the buildpacks to the builder and runs them", func() {
								packageFileLocation := filepath.Join(
									tmpDir,
									fmt.Sprintf("buildpack-%s.cnb", h.RandString(8)),
								)

								packageTomlPath := generatePackageTomlWithOS(t, assert, pack, tmpDir, "package_for_build_cmd.toml", imageManager.HostOS())
								packageFile := buildpacks.NewPackageFile(
									t,
									pack,
									packageFileLocation,
									packageTomlPath,
									buildpacks.WithRequiredBuildpacks(
										buildpacks.BpFolderSimpleLayersParent,
										buildpacks.BpFolderSimpleLayers,
									),
								)

								buildpackManager.PrepareBuildModules(tmpDir, packageFile)

								output := pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--buildpack", packageFileLocation,
								)

								assertOutput := assertions.NewOutputAssertionManager(t, output)
								assertOutput.ReportsAddingBuildpack(
									"simple/layers/parent",
									"simple-layers-parent-version",
								)
								assertOutput.ReportsAddingBuildpack("simple/layers", "simple-layers-version")
								assertOutput.ReportsSuccessfulImageBuild(repoName)

								assertBuildpackOutput := assertions.NewTestBuildpackOutputAssertionManager(t, output)
								assertBuildpackOutput.ReportsBuildStep("Simple Layers Buildpack")
							})
						})

						when("the buildpack stack doesn't match the builder", func() {
							var otherStackBuilderTgz string

							it.Before(func() {
								// The Platform API is new if pack is new AND the lifecycle is new
								// Therefore skip if pack is old OR the lifecycle is old
								h.SkipIf(t,
									pack.SupportsFeature(invoke.StackValidation) ||
										api.MustParse(lifecycle.LatestPlatformAPIVersion()).LessThan("0.12"), "")
								otherStackBuilderTgz = h.CreateTGZ(t, filepath.Join(bpDir, "other-stack-buildpack"), "./", 0755)
							})

							it.After(func() {
								h.SkipIf(t,
									pack.SupportsFeature(invoke.StackValidation) ||
										api.MustParse(lifecycle.LatestPlatformAPIVersion()).LessThan("0.12"), "")
								assert.Succeeds(os.Remove(otherStackBuilderTgz))
							})

							it("succeeds", func() {
								_, err := pack.Run(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--buildpack", otherStackBuilderTgz,
								)
								assert.Nil(err)
							})

							when("platform API < 0.12", func() {
								it.Before(func() {
									// The Platform API is old if pack is old OR the lifecycle is old
									// Therefore skip if pack is new AND the lifecycle is new
									h.SkipIf(t,
										!pack.SupportsFeature(invoke.StackValidation) &&
											api.MustParse(lifecycle.LatestPlatformAPIVersion()).AtLeast("0.12"), "")
								})

								it("errors", func() {
									output, err := pack.Run(
										"build", repoName,
										"-p", filepath.Join("testdata", "mock_app"),
										"--buildpack", otherStackBuilderTgz,
									)

									assert.NotNil(err)
									assert.Contains(output, "other/stack/bp")
									assert.Contains(output, "other-stack-version")
									assert.Contains(output, "does not support stack 'pack.test.stack'")
								})
							})
						})
					})

					when("--env-file", func() {
						var envPath string

						it.Before(func() {
							envfile, err := os.CreateTemp("", "envfile")
							assert.Nil(err)
							defer envfile.Close()

							err = os.Setenv("ENV2_CONTENTS", "Env2 Layer Contents From Environment")
							assert.Nil(err)
							envfile.WriteString(`
            DETECT_ENV_BUILDPACK=true
			ENV1_CONTENTS=Env1 Layer Contents From File
			ENV2_CONTENTS
			`)
							envPath = envfile.Name()
						})

						it.After(func() {
							assert.Succeeds(os.Unsetenv("ENV2_CONTENTS"))
							assert.Succeeds(os.RemoveAll(envPath))
						})

						it("provides the env vars to the build and detect steps", func() {
							output := pack.RunSuccessfully(
								"build", repoName,
								"-p", filepath.Join("testdata", "mock_app"),
								"--env-file", envPath,
							)

							assertions.NewOutputAssertionManager(t, output).ReportsSuccessfulImageBuild(repoName)
							assertImage.RunsWithOutput(
								repoName,
								"Env2 Layer Contents From Environment",
								"Env1 Layer Contents From File",
							)
						})
					})

					when("--env", func() {
						it.Before(func() {
							assert.Succeeds(os.Setenv("ENV2_CONTENTS", "Env2 Layer Contents From Environment"))
						})

						it.After(func() {
							assert.Succeeds(os.Unsetenv("ENV2_CONTENTS"))
						})

						it("provides the env vars to the build and detect steps", func() {
							output := pack.RunSuccessfully(
								"build", repoName,
								"-p", filepath.Join("testdata", "mock_app"),
								"--env", "DETECT_ENV_BUILDPACK=true",
								"--env", `ENV1_CONTENTS="Env1 Layer Contents From Command Line"`,
								"--env", "ENV2_CONTENTS",
							)

							assertions.NewOutputAssertionManager(t, output).ReportsSuccessfulImageBuild(repoName)
							assertImage.RunsWithOutput(
								repoName,
								"Env2 Layer Contents From Environment",
								"Env1 Layer Contents From Command Line",
							)
						})
					})

					when("--run-image", func() {
						var runImageName string

						when("the run-image has the correct stack ID", func() {
							it.Before(func() {
								user := func() string {
									if imageManager.HostOS() == "windows" {
										return "ContainerAdministrator"
									}

									return "root"
								}

								runImageName = h.CreateImageOnRemote(t, dockerCli, registryConfig, "custom-run-image"+h.RandString(10), fmt.Sprintf(`
													FROM %s
													USER %s
													RUN echo "custom-run" > /custom-run.txt
													USER pack
												`, runImage, user()))
							})

							it.After(func() {
								imageManager.CleanupImages(runImageName)
							})

							it("uses the run image as the base image", func() {
								output := pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--run-image", runImageName,
								)
								assertOutput := assertions.NewOutputAssertionManager(t, output)
								assertOutput.ReportsSuccessfulImageBuild(repoName)
								assertOutput.ReportsPullingImage(runImageName)

								t.Log("app is runnable")
								assertImage.RunsWithOutput(
									repoName,
									"Launch Dep Contents",
									"Cached Dep Contents",
								)

								t.Log("uses the run image as the base image")
								assertImage.HasBaseImage(repoName, runImageName)
							})
						})

						when("the run image has the wrong stack ID", func() {
							it.Before(func() {
								runImageName = h.CreateImageOnRemote(t, dockerCli, registryConfig, "custom-run-image"+h.RandString(10), fmt.Sprintf(`
													FROM %s
													LABEL io.buildpacks.stack.id=other.stack.id
													USER pack
												`, runImage))

							})

							it.After(func() {
								imageManager.CleanupImages(runImageName)
							})

							it("fails with a message", func() {
								output, err := pack.Run(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--run-image", runImageName,
								)
								assert.NotNil(err)

								assertOutput := assertions.NewOutputAssertionManager(t, output)
								assertOutput.ReportsRunImageStackNotMatchingBuilder(
									"other.stack.id",
									"pack.test.stack",
								)
							})
						})
					})

					when("--publish", func() {
						it("creates image on the registry", func() {
							buildArgs := []string{
								repoName,
								"-p", filepath.Join("testdata", "mock_app"),
								"--publish",
							}
							if imageManager.HostOS() != "windows" {
								buildArgs = append(buildArgs, "--network", "host")
							}

							output := pack.RunSuccessfully("build", buildArgs...)
							assertions.NewOutputAssertionManager(t, output).ReportsSuccessfulImageBuild(repoName)

							t.Log("checking that registry has contents")
							assertImage.ExistsInRegistryCatalog(repo)

							cmdName := "inspect"
							if !pack.Supports("inspect") {
								cmdName = "inspect-image"
							}

							t.Log("inspect-image")
							var (
								webCommand      string
								helloCommand    string
								helloArgs       []string
								helloArgsPrefix string
								imageWorkdir    string
							)
							if imageManager.HostOS() == "windows" {
								webCommand = ".\\run"
								helloCommand = "cmd"
								helloArgs = []string{"/c", "echo hello world"}
								helloArgsPrefix = " "
								imageWorkdir = "c:\\workspace"
							} else {
								webCommand = "./run"
								helloCommand = "echo"
								helloArgs = []string{"hello", "world"}
								helloArgsPrefix = ""
								imageWorkdir = "/workspace"
							}
							formats := []compareFormat{
								{
									extension:   "json",
									compareFunc: assert.EqualJSON,
									outputArg:   "json",
								},
								{
									extension:   "yaml",
									compareFunc: assert.EqualYAML,
									outputArg:   "yaml",
								},
								{
									extension:   "toml",
									compareFunc: assert.EqualTOML,
									outputArg:   "toml",
								},
							}

							for _, format := range formats {
								t.Logf("inspecting image %s format", format.outputArg)

								output = pack.RunSuccessfully(cmdName, repoName, "--output", format.outputArg)

								expectedOutput := pack.FixtureManager().TemplateFixture(
									fmt.Sprintf("inspect_image_published_output.%s", format.extension),
									map[string]interface{}{
										"image_name":           repoName,
										"base_image_ref":       strings.Join([]string{runImageMirror, h.Digest(t, runImageMirror)}, "@"),
										"base_image_top_layer": h.TopLayerDiffID(t, runImageMirror),
										"run_image_mirror":     runImageMirror,
										"web_command":          webCommand,
										"hello_command":        helloCommand,
										"hello_args":           helloArgs,
										"hello_args_prefix":    helloArgsPrefix,
										"image_workdir":        imageWorkdir,
									},
								)

								format.compareFunc(output, expectedOutput)
							}

							imageManager.PullImage(repoName, registryConfig.RegistryAuth())

							t.Log("app is runnable")
							assertImage.RunsWithOutput(
								repoName,
								"Launch Dep Contents",
								"Cached Dep Contents",
							)
						})

						when("additional tags are specified with --tag", func() {
							var additionalRepo string
							var additionalRepoName string

							it.Before(func() {
								additionalRepo = fmt.Sprintf("%s_additional", repo)
								additionalRepoName = fmt.Sprintf("%s_additional", repoName)
							})
							it.After(func() {
								imageManager.CleanupImages(additionalRepoName)
							})
							it("creates additional tags on the registry", func() {
								buildArgs := []string{
									repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--publish",
									"--tag", additionalRepoName,
								}

								if imageManager.HostOS() != "windows" {
									buildArgs = append(buildArgs, "--network", "host")
								}

								output := pack.RunSuccessfully("build", buildArgs...)
								assertions.NewOutputAssertionManager(t, output).ReportsSuccessfulImageBuild(repoName)

								t.Log("checking that registry has contents")
								assertImage.ExistsInRegistryCatalog(repo)
								assertImage.ExistsInRegistryCatalog(additionalRepo)

								imageManager.PullImage(repoName, registryConfig.RegistryAuth())
								imageManager.PullImage(additionalRepoName, registryConfig.RegistryAuth())

								t.Log("additional app is runnable")
								assertImage.RunsWithOutput(
									additionalRepoName,
									"Launch Dep Contents",
									"Cached Dep Contents",
								)

								imageDigest := h.Digest(t, repoName)
								additionalDigest := h.Digest(t, additionalRepoName)

								assert.Equal(imageDigest, additionalDigest)
							})

						})
					})

					when("--cache-image", func() {
						var cacheImageName string
						it.Before(func() {
							cacheImageName = fmt.Sprintf("%s-cache", repoName)
						})

						it("creates image and cache image on the registry", func() {
							buildArgs := []string{
								repoName,
								"-p", filepath.Join("testdata", "mock_app"),
								"--publish",
								"--cache-image",
								cacheImageName,
							}
							if imageManager.HostOS() != "windows" {
								buildArgs = append(buildArgs, "--network", "host")
							}

							output := pack.RunSuccessfully("build", buildArgs...)
							assertions.NewOutputAssertionManager(t, output).ReportsSuccessfulImageBuild(repoName)

							cacheImageRef, err := name.ParseReference(cacheImageName, name.WeakValidation)
							assert.Nil(err)

							t.Log("checking that registry has contents")
							assertImage.CanBePulledFromRegistry(repoName)
							if imageManager.HostOS() == "windows" {
								// Cache images are automatically Linux container images, and therefore can't be pulled
								// and inspected correctly on WCOW systems
								// https://github.com/buildpacks/lifecycle/issues/529
								imageManager.PullImage(cacheImageRef.Name(), registryConfig.RegistryAuth())
							} else {
								assertImage.CanBePulledFromRegistry(cacheImageRef.Name())
							}

							defer imageManager.CleanupImages(cacheImageRef.Name())
						})
					})

					when("--cache with options for build cache as image", func() {
						var cacheImageName, cacheFlags string
						it.Before(func() {
							cacheImageName = fmt.Sprintf("%s-cache", repoName)
							cacheFlags = fmt.Sprintf("type=build;format=image;name=%s", cacheImageName)
						})

						it("creates image and cache image on the registry", func() {
							buildArgs := []string{
								repoName,
								"-p", filepath.Join("testdata", "mock_app"),
								"--publish",
								"--cache",
								cacheFlags,
							}
							if imageManager.HostOS() != "windows" {
								buildArgs = append(buildArgs, "--network", "host")
							}

							output := pack.RunSuccessfully("build", buildArgs...)
							assertions.NewOutputAssertionManager(t, output).ReportsSuccessfulImageBuild(repoName)

							cacheImageRef, err := name.ParseReference(cacheImageName, name.WeakValidation)
							assert.Nil(err)

							t.Log("checking that registry has contents")
							assertImage.CanBePulledFromRegistry(repoName)
							if imageManager.HostOS() == "windows" {
								// Cache images are automatically Linux container images, and therefore can't be pulled
								// and inspected correctly on WCOW systems
								// https://github.com/buildpacks/lifecycle/issues/529
								imageManager.PullImage(cacheImageRef.Name(), registryConfig.RegistryAuth())
							} else {
								assertImage.CanBePulledFromRegistry(cacheImageRef.Name())
							}

							defer imageManager.CleanupImages(cacheImageRef.Name())
						})
					})

					when("--cache with options for build cache as bind", func() {
						var bindCacheDir, cacheFlags string
						it.Before(func() {
							h.SkipIf(t, !pack.SupportsFeature(invoke.Cache), "")
							cacheBindName := strings.ReplaceAll(strings.ReplaceAll(fmt.Sprintf("%s-bind", repoName), string(filepath.Separator), "-"), ":", "-")
							var err error
							bindCacheDir, err = os.MkdirTemp("", cacheBindName)
							assert.Nil(err)
							cacheFlags = fmt.Sprintf("type=build;format=bind;source=%s", bindCacheDir)
						})

						it("creates image and cache image on the registry", func() {
							buildArgs := []string{
								repoName,
								"-p", filepath.Join("testdata", "mock_app"),
								"--cache",
								cacheFlags,
							}

							output := pack.RunSuccessfully("build", buildArgs...)
							assertions.NewOutputAssertionManager(t, output).ReportsSuccessfulImageBuild(repoName)

							t.Log("checking that bind mount has cache contents")
							assert.FileExists(fmt.Sprintf("%s/committed", bindCacheDir))
							defer os.RemoveAll(bindCacheDir)
						})
					})

					when("ctrl+c", func() {
						it("stops the execution", func() {
							var buf = new(bytes.Buffer)
							command := pack.StartWithWriter(
								buf,
								"build", repoName,
								"-p", filepath.Join("testdata", "mock_app"),
							)

							go command.TerminateAtStep("DETECTING")

							err := command.Wait()
							assert.NotNil(err)
							assert.NotContains(buf.String(), "Successfully built image")
						})
					})

					when("--descriptor", func() {

						when("using a included buildpack", func() {
							var tempAppDir, tempWorkingDir, origWorkingDir string
							it.Before(func() {
								h.SkipIf(t, runtime.GOOS == "windows", "buildpack directories not supported on windows")

								var err error
								tempAppDir, err = os.MkdirTemp("", "descriptor-app")
								assert.Nil(err)

								tempWorkingDir, err = os.MkdirTemp("", "descriptor-app")
								assert.Nil(err)

								origWorkingDir, err = os.Getwd()
								assert.Nil(err)

								// Create test directories and files:
								//
								// ├── cookie.jar
								// ├── descriptor-buildpack/...
								// ├── media
								// │   ├── mountain.jpg
								// │   └── person.png
								// └── test.sh
								assert.Succeeds(os.Mkdir(filepath.Join(tempAppDir, "descriptor-buildpack"), os.ModePerm))
								h.RecursiveCopy(t, filepath.Join(bpDir, "descriptor-buildpack"), filepath.Join(tempAppDir, "descriptor-buildpack"))

								err = os.Mkdir(filepath.Join(tempAppDir, "media"), 0755)
								assert.Nil(err)
								err = os.WriteFile(filepath.Join(tempAppDir, "media", "mountain.jpg"), []byte("fake image bytes"), 0755)
								assert.Nil(err)
								err = os.WriteFile(filepath.Join(tempAppDir, "media", "person.png"), []byte("fake image bytes"), 0755)
								assert.Nil(err)

								err = os.WriteFile(filepath.Join(tempAppDir, "cookie.jar"), []byte("chocolate chip"), 0755)
								assert.Nil(err)
								err = os.WriteFile(filepath.Join(tempAppDir, "test.sh"), []byte("echo test"), 0755)
								assert.Nil(err)

								projectToml := `
[project]
name = "exclude test"
[[project.licenses]]
type = "MIT"
[build]
exclude = [ "*.sh", "media/person.png", "descriptor-buildpack" ]

[[build.buildpacks]]
uri = "descriptor-buildpack"
`
								excludeDescriptorPath := filepath.Join(tempAppDir, "project.toml")
								err = os.WriteFile(excludeDescriptorPath, []byte(projectToml), 0755)
								assert.Nil(err)

								// set working dir to be outside of the app we are building
								assert.Succeeds(os.Chdir(tempWorkingDir))
							})

							it.After(func() {
								os.RemoveAll(tempAppDir)
								if origWorkingDir != "" {
									assert.Succeeds(os.Chdir(origWorkingDir))
								}
							})
							it("uses buildpack specified by descriptor", func() {
								output := pack.RunSuccessfully(
									"build",
									repoName,
									"-p", tempAppDir,
								)
								assert.NotContains(output, "person.png")
								assert.NotContains(output, "test.sh")

							})
						})

						when("exclude and include", func() {
							var buildpackTgz, tempAppDir string

							it.Before(func() {
								buildpackTgz = h.CreateTGZ(t, filepath.Join(bpDir, "descriptor-buildpack"), "./", 0755)

								var err error
								tempAppDir, err = os.MkdirTemp("", "descriptor-app")
								assert.Nil(err)

								// Create test directories and files:
								//
								// ├── cookie.jar
								// ├── other-cookie.jar
								// ├── nested-cookie.jar
								// ├── nested
								// │   └── nested-cookie.jar
								// ├── secrets
								// │   ├── api_keys.json
								// |   |── user_token
								// ├── media
								// │   ├── mountain.jpg
								// │   └── person.png
								// └── test.sh

								err = os.Mkdir(filepath.Join(tempAppDir, "secrets"), 0755)
								assert.Nil(err)
								err = os.WriteFile(filepath.Join(tempAppDir, "secrets", "api_keys.json"), []byte("{}"), 0755)
								assert.Nil(err)
								err = os.WriteFile(filepath.Join(tempAppDir, "secrets", "user_token"), []byte("token"), 0755)
								assert.Nil(err)

								err = os.Mkdir(filepath.Join(tempAppDir, "nested"), 0755)
								assert.Nil(err)
								err = os.WriteFile(filepath.Join(tempAppDir, "nested", "nested-cookie.jar"), []byte("chocolate chip"), 0755)
								assert.Nil(err)

								err = os.WriteFile(filepath.Join(tempAppDir, "other-cookie.jar"), []byte("chocolate chip"), 0755)
								assert.Nil(err)

								err = os.WriteFile(filepath.Join(tempAppDir, "nested-cookie.jar"), []byte("chocolate chip"), 0755)
								assert.Nil(err)

								err = os.Mkdir(filepath.Join(tempAppDir, "media"), 0755)
								assert.Nil(err)
								err = os.WriteFile(filepath.Join(tempAppDir, "media", "mountain.jpg"), []byte("fake image bytes"), 0755)
								assert.Nil(err)
								err = os.WriteFile(filepath.Join(tempAppDir, "media", "person.png"), []byte("fake image bytes"), 0755)
								assert.Nil(err)

								err = os.WriteFile(filepath.Join(tempAppDir, "cookie.jar"), []byte("chocolate chip"), 0755)
								assert.Nil(err)
								err = os.WriteFile(filepath.Join(tempAppDir, "test.sh"), []byte("echo test"), 0755)
								assert.Nil(err)
							})

							it.After(func() {
								assert.Succeeds(os.RemoveAll(tempAppDir))
							})

							it("should exclude ALL specified files and directories", func() {
								projectToml := `
[project]
name = "exclude test"
[[project.licenses]]
type = "MIT"
[build]
exclude = [ "*.sh", "secrets/", "media/metadata", "/other-cookie.jar" ,"/nested-cookie.jar"]
`
								excludeDescriptorPath := filepath.Join(tempAppDir, "exclude.toml")
								err := os.WriteFile(excludeDescriptorPath, []byte(projectToml), 0755)
								assert.Nil(err)

								output := pack.RunSuccessfully(
									"build",
									repoName,
									"-p", tempAppDir,
									"--buildpack", buildpackTgz,
									"--descriptor", excludeDescriptorPath,
								)
								assert.NotContains(output, "api_keys.json")
								assert.NotContains(output, "user_token")
								assert.NotContains(output, "test.sh")
								assert.NotContains(output, "other-cookie.jar")

								assert.Contains(output, "cookie.jar")
								assert.Contains(output, "nested-cookie.jar")
								assert.Contains(output, "mountain.jpg")
								assert.Contains(output, "person.png")
							})

							it("should ONLY include specified files and directories", func() {
								projectToml := `
[project]
name = "include test"
[[project.licenses]]
type = "MIT"
[build]
include = [ "*.jar", "media/mountain.jpg", "/media/person.png", ]
`
								includeDescriptorPath := filepath.Join(tempAppDir, "include.toml")
								err := os.WriteFile(includeDescriptorPath, []byte(projectToml), 0755)
								assert.Nil(err)

								output := pack.RunSuccessfully(
									"build",
									repoName,
									"-p", tempAppDir,
									"--buildpack", buildpackTgz,
									"--descriptor", includeDescriptorPath,
								)
								assert.NotContains(output, "api_keys.json")
								assert.NotContains(output, "user_token")
								assert.NotContains(output, "test.sh")

								assert.Contains(output, "cookie.jar")
								assert.Contains(output, "mountain.jpg")
								assert.Contains(output, "person.png")
							})
						})
					})

					when("--creation-time", func() {
						when("provided as 'now'", func() {
							it("image has create time of the current time", func() {
								expectedTime := time.Now()
								pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--creation-time", "now",
								)
								assertImage.HasCreateTime(repoName, expectedTime)
							})
						})

						when("provided as unix timestamp", func() {
							it("image has create time of the time that was provided", func() {
								pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
									"--creation-time", "1566172801",
								)
								expectedTime, err := time.Parse("2006-01-02T03:04:05Z", "2019-08-19T00:00:01Z")
								h.AssertNil(t, err)
								assertImage.HasCreateTime(repoName, expectedTime)
							})
						})

						when("not provided", func() {
							it("image has create time of Jan 1, 1980", func() {
								pack.RunSuccessfully(
									"build", repoName,
									"-p", filepath.Join("testdata", "mock_app"),
								)
								expectedTime, err := time.Parse("2006-01-02T03:04:05Z", "1980-01-01T00:00:01Z")
								h.AssertNil(t, err)
								assertImage.HasCreateTime(repoName, expectedTime)
							})
						})
					})
				})
			})

			when("inspecting builder", func() {
				when("inspecting a nested builder", func() {
					it.Before(func() {
						// create our nested builder
						h.SkipIf(t, imageManager.HostOS() == "windows", "These tests are not yet compatible with Windows-based containers")

						// create a task, handled by a 'task manager' which executes our pack commands during tests.
						// looks like this is used to de-dup tasks
						key := taskKey(
							"create-complex-builder",
							append(
								[]string{runImageMirror, createBuilderPackConfig.Path(), lifecycle.Identifier()},
								createBuilderPackConfig.FixturePaths()...,
							)...,
						)
						// run task on taskmanager and save output, in case there are future calls to the same task
						// likely all our changes need to go on the createBuilderPack.
						value, err := suiteManager.RunTaskOnceString(key, func() (string, error) {
							return createComplexBuilder(
								t,
								assert,
								createBuilderPack,
								lifecycle,
								buildpackManager,
								runImageMirror,
							)
						})
						assert.Nil(err)

						// register task to be run to 'clean up' a task
						suiteManager.RegisterCleanUp("clean-"+key, func() error {
							imageManager.CleanupImages(value)
							return nil
						})
						builderName = value

						output := pack.RunSuccessfully(
							"config", "run-image-mirrors", "add", "pack-test/run", "--mirror", "some-registry.com/pack-test/run1")
						assertOutput := assertions.NewOutputAssertionManager(t, output)
						assertOutput.ReportsSuccesfulRunImageMirrorsAdd("pack-test/run", "some-registry.com/pack-test/run1")
					})

					it("displays nested Detection Order groups", func() {
						var output string
						if pack.Supports("builder inspect") {
							output = pack.RunSuccessfully("builder", "inspect", builderName)
						} else {
							output = pack.RunSuccessfully("inspect-builder", builderName)
						}

						deprecatedBuildpackAPIs,
							supportedBuildpackAPIs,
							deprecatedPlatformAPIs,
							supportedPlatformAPIs := lifecycle.OutputForAPIs()

						expectedOutput := pack.FixtureManager().TemplateVersionedFixture(
							"inspect_%s_builder_nested_output.txt",
							createBuilderPack.SanitizedVersion(),
							"inspect_builder_nested_output.txt",
							map[string]interface{}{
								"builder_name":              builderName,
								"lifecycle_version":         lifecycle.Version(),
								"deprecated_buildpack_apis": deprecatedBuildpackAPIs,
								"supported_buildpack_apis":  supportedBuildpackAPIs,
								"deprecated_platform_apis":  deprecatedPlatformAPIs,
								"supported_platform_apis":   supportedPlatformAPIs,
								"run_image_mirror":          runImageMirror,
								"pack_version":              createBuilderPack.Version(),
								"trusted":                   "No",

								// set previous pack template fields
								"buildpack_api_version": lifecycle.EarliestBuildpackAPIVersion(),
								"platform_api_version":  lifecycle.EarliestPlatformAPIVersion(),
							},
						)

						assert.TrimmedEq(output, expectedOutput)
					})

					it("provides nested detection output up to depth", func() {
						depth := "1"
						var output string
						if pack.Supports("builder inspect") {
							output = pack.RunSuccessfully("builder", "inspect", "--depth", depth, builderName)
						} else {
							output = pack.RunSuccessfully("inspect-builder", "--depth", depth, builderName)
						}

						deprecatedBuildpackAPIs,
							supportedBuildpackAPIs,
							deprecatedPlatformAPIs,
							supportedPlatformAPIs := lifecycle.OutputForAPIs()

						expectedOutput := pack.FixtureManager().TemplateVersionedFixture(
							"inspect_%s_builder_nested_depth_2_output.txt",
							createBuilderPack.SanitizedVersion(),
							"inspect_builder_nested_depth_2_output.txt",
							map[string]interface{}{
								"builder_name":              builderName,
								"lifecycle_version":         lifecycle.Version(),
								"deprecated_buildpack_apis": deprecatedBuildpackAPIs,
								"supported_buildpack_apis":  supportedBuildpackAPIs,
								"deprecated_platform_apis":  deprecatedPlatformAPIs,
								"supported_platform_apis":   supportedPlatformAPIs,
								"run_image_mirror":          runImageMirror,
								"pack_version":              createBuilderPack.Version(),
								"trusted":                   "No",

								// set previous pack template fields
								"buildpack_api_version": lifecycle.EarliestBuildpackAPIVersion(),
								"platform_api_version":  lifecycle.EarliestPlatformAPIVersion(),
							},
						)

						assert.TrimmedEq(output, expectedOutput)
					})

					when("output format is toml", func() {
						it("prints builder information in toml format", func() {
							var output string
							if pack.Supports("builder inspect") {
								output = pack.RunSuccessfully("builder", "inspect", builderName, "--output", "toml")
							} else {
								output = pack.RunSuccessfully("inspect-builder", builderName, "--output", "toml")
							}

							err := toml.NewDecoder(strings.NewReader(string(output))).Decode(&struct{}{})
							assert.Nil(err)

							deprecatedBuildpackAPIs,
								supportedBuildpackAPIs,
								deprecatedPlatformAPIs,
								supportedPlatformAPIs := lifecycle.TOMLOutputForAPIs()

							expectedOutput := pack.FixtureManager().TemplateVersionedFixture(
								"inspect_%s_builder_nested_output_toml.txt",
								createBuilderPack.SanitizedVersion(),
								"inspect_builder_nested_output_toml.txt",
								map[string]interface{}{
									"builder_name":              builderName,
									"lifecycle_version":         lifecycle.Version(),
									"deprecated_buildpack_apis": deprecatedBuildpackAPIs,
									"supported_buildpack_apis":  supportedBuildpackAPIs,
									"deprecated_platform_apis":  deprecatedPlatformAPIs,
									"supported_platform_apis":   supportedPlatformAPIs,
									"run_image_mirror":          runImageMirror,
									"pack_version":              createBuilderPack.Version(),
								},
							)

							assert.TrimmedEq(string(output), expectedOutput)
						})
					})

					when("output format is yaml", func() {
						it("prints builder information in yaml format", func() {
							var output string
							if pack.Supports("builder inspect") {
								output = pack.RunSuccessfully("builder", "inspect", builderName, "--output", "yaml")
							} else {
								output = pack.RunSuccessfully("inspect-builder", builderName, "--output", "yaml")
							}

							err := yaml.Unmarshal([]byte(output), &struct{}{})
							assert.Nil(err)

							deprecatedBuildpackAPIs,
								supportedBuildpackAPIs,
								deprecatedPlatformAPIs,
								supportedPlatformAPIs := lifecycle.YAMLOutputForAPIs(14)

							expectedOutput := pack.FixtureManager().TemplateVersionedFixture(
								"inspect_%s_builder_nested_output_yaml.txt",
								createBuilderPack.SanitizedVersion(),
								"inspect_builder_nested_output_yaml.txt",
								map[string]interface{}{
									"builder_name":              builderName,
									"lifecycle_version":         lifecycle.Version(),
									"deprecated_buildpack_apis": deprecatedBuildpackAPIs,
									"supported_buildpack_apis":  supportedBuildpackAPIs,
									"deprecated_platform_apis":  deprecatedPlatformAPIs,
									"supported_platform_apis":   supportedPlatformAPIs,
									"run_image_mirror":          runImageMirror,
									"pack_version":              createBuilderPack.Version(),
								},
							)

							assert.TrimmedEq(string(output), expectedOutput)
						})
					})

					when("output format is json", func() {
						it("prints builder information in json format", func() {
							var output string
							if pack.Supports("builder inspect") {
								output = pack.RunSuccessfully("builder", "inspect", builderName, "--output", "json")
							} else {
								output = pack.RunSuccessfully("inspect-builder", builderName, "--output", "json")
							}

							err := json.Unmarshal([]byte(output), &struct{}{})
							assert.Nil(err)

							var prettifiedOutput bytes.Buffer
							err = json.Indent(&prettifiedOutput, []byte(output), "", "  ")
							assert.Nil(err)

							deprecatedBuildpackAPIs,
								supportedBuildpackAPIs,
								deprecatedPlatformAPIs,
								supportedPlatformAPIs := lifecycle.JSONOutputForAPIs(8)

							expectedOutput := pack.FixtureManager().TemplateVersionedFixture(
								"inspect_%s_builder_nested_output_json.txt",
								createBuilderPack.SanitizedVersion(),
								"inspect_builder_nested_output_json.txt",
								map[string]interface{}{
									"builder_name":              builderName,
									"lifecycle_version":         lifecycle.Version(),
									"deprecated_buildpack_apis": deprecatedBuildpackAPIs,
									"supported_buildpack_apis":  supportedBuildpackAPIs,
									"deprecated_platform_apis":  deprecatedPlatformAPIs,
									"supported_platform_apis":   supportedPlatformAPIs,
									"run_image_mirror":          runImageMirror,
									"pack_version":              createBuilderPack.Version(),
								},
							)

							assert.Equal(prettifiedOutput.String(), expectedOutput)
						})
					})
				})

				it("displays configuration for a builder (local and remote)", func() {
					output := pack.RunSuccessfully(
						"config", "run-image-mirrors", "add", "pack-test/run", "--mirror", "some-registry.com/pack-test/run1",
					)
					assertOutput := assertions.NewOutputAssertionManager(t, output)
					assertOutput.ReportsSuccesfulRunImageMirrorsAdd("pack-test/run", "some-registry.com/pack-test/run1")

					if pack.Supports("builder inspect") {
						output = pack.RunSuccessfully("builder", "inspect", builderName)
					} else {
						output = pack.RunSuccessfully("inspect-builder", builderName)
					}

					deprecatedBuildpackAPIs,
						supportedBuildpackAPIs,
						deprecatedPlatformAPIs,
						supportedPlatformAPIs := lifecycle.OutputForAPIs()

					expectedOutput := pack.FixtureManager().TemplateVersionedFixture(
						"inspect_%s_builder_output.txt",
						createBuilderPack.SanitizedVersion(),
						"inspect_builder_output.txt",
						map[string]interface{}{
							"builder_name":              builderName,
							"lifecycle_version":         lifecycle.Version(),
							"deprecated_buildpack_apis": deprecatedBuildpackAPIs,
							"supported_buildpack_apis":  supportedBuildpackAPIs,
							"deprecated_platform_apis":  deprecatedPlatformAPIs,
							"supported_platform_apis":   supportedPlatformAPIs,
							"run_image_mirror":          runImageMirror,
							"pack_version":              createBuilderPack.Version(),
							"trusted":                   "No",

							// set previous pack template fields
							"buildpack_api_version": lifecycle.EarliestBuildpackAPIVersion(),
							"platform_api_version":  lifecycle.EarliestPlatformAPIVersion(),
						},
					)

					assert.TrimmedEq(output, expectedOutput)
				})

				it("indicates builder is trusted", func() {
					pack.JustRunSuccessfully("config", "trusted-builders", "add", builderName)
					pack.JustRunSuccessfully("config", "run-image-mirrors", "add", "pack-test/run", "--mirror", "some-registry.com/pack-test/run1")

					var output string
					if pack.Supports("builder inspect") {
						output = pack.RunSuccessfully("builder", "inspect", builderName)
					} else {
						output = pack.RunSuccessfully("inspect-builder", builderName)
					}

					deprecatedBuildpackAPIs,
						supportedBuildpackAPIs,
						deprecatedPlatformAPIs,
						supportedPlatformAPIs := lifecycle.OutputForAPIs()

					expectedOutput := pack.FixtureManager().TemplateVersionedFixture(
						"inspect_%s_builder_output.txt",
						createBuilderPack.SanitizedVersion(),
						"inspect_builder_output.txt",
						map[string]interface{}{
							"builder_name":              builderName,
							"lifecycle_version":         lifecycle.Version(),
							"deprecated_buildpack_apis": deprecatedBuildpackAPIs,
							"supported_buildpack_apis":  supportedBuildpackAPIs,
							"deprecated_platform_apis":  deprecatedPlatformAPIs,
							"supported_platform_apis":   supportedPlatformAPIs,
							"run_image_mirror":          runImageMirror,
							"pack_version":              createBuilderPack.Version(),
							"trusted":                   "Yes",

							// set previous pack template fields
							"buildpack_api_version": lifecycle.EarliestBuildpackAPIVersion(),
							"platform_api_version":  lifecycle.EarliestPlatformAPIVersion(),
						},
					)

					assert.TrimmedEq(output, expectedOutput)
				})
			})

			when("rebase", func() {
				var repoName, runBefore, origID string
				var buildRunImage func(string, string, string)

				it.Before(func() {
					pack.JustRunSuccessfully("config", "trusted-builders", "add", builderName)

					repoName = registryConfig.RepoName("some-org/" + h.RandString(10))
					runBefore = registryConfig.RepoName("run-before/" + h.RandString(10))

					buildRunImage = func(newRunImage, contents1, contents2 string) {
						user := func() string {
							if imageManager.HostOS() == "windows" {
								return "ContainerAdministrator"
							}

							return "root"
						}

						h.CreateImage(t, dockerCli, newRunImage, fmt.Sprintf(`
													FROM %s
													USER %s
													RUN echo %s > /contents1.txt
													RUN echo %s > /contents2.txt
													USER pack
												`, runImage, user(), contents1, contents2))
					}

					buildRunImage(runBefore, "contents-before-1", "contents-before-2")
					pack.RunSuccessfully(
						"build", repoName,
						"-p", filepath.Join("testdata", "mock_app"),
						"--builder", builderName,
						"--run-image", runBefore,
						"--pull-policy", "never",
					)
					origID = h.ImageID(t, repoName)
					assertImage.RunsWithOutput(
						repoName,
						"contents-before-1",
						"contents-before-2",
					)
				})

				it.After(func() {
					imageManager.CleanupImages(origID, repoName, runBefore)
					ref, err := name.ParseReference(repoName, name.WeakValidation)
					assert.Nil(err)
					buildCacheVolume := cache.NewVolumeCache(ref, cache.CacheInfo{}, "build", dockerCli)
					launchCacheVolume := cache.NewVolumeCache(ref, cache.CacheInfo{}, "launch", dockerCli)
					assert.Succeeds(buildCacheVolume.Clear(context.TODO()))
					assert.Succeeds(launchCacheVolume.Clear(context.TODO()))
				})

				when("daemon", func() {
					when("--run-image", func() {
						var runAfter string

						it.Before(func() {
							runAfter = registryConfig.RepoName("run-after/" + h.RandString(10))
							buildRunImage(runAfter, "contents-after-1", "contents-after-2")
						})

						it.After(func() {
							imageManager.CleanupImages(runAfter)
						})

						it("uses provided run image", func() {
							output := pack.RunSuccessfully(
								"rebase", repoName,
								"--run-image", runAfter,
								"--pull-policy", "never",
							)

							assert.Contains(output, fmt.Sprintf("Successfully rebased image '%s'", repoName))
							assertImage.RunsWithOutput(
								repoName,
								"contents-after-1",
								"contents-after-2",
							)
						})
					})

					when("local config has a mirror", func() {
						var localRunImageMirror string

						it.Before(func() {
							localRunImageMirror = registryConfig.RepoName("run-after/" + h.RandString(10))
							buildRunImage(localRunImageMirror, "local-mirror-after-1", "local-mirror-after-2")
							pack.JustRunSuccessfully("config", "run-image-mirrors", "add", runImage, "-m", localRunImageMirror)
						})

						it.After(func() {
							imageManager.CleanupImages(localRunImageMirror)
						})

						it("prefers the local mirror", func() {
							output := pack.RunSuccessfully("rebase", repoName, "--pull-policy", "never")

							assertOutput := assertions.NewOutputAssertionManager(t, output)
							assertOutput.ReportsSelectingRunImageMirrorFromLocalConfig(localRunImageMirror)
							assertOutput.ReportsSuccessfulRebase(repoName)
							assertImage.RunsWithOutput(
								repoName,
								"local-mirror-after-1",
								"local-mirror-after-2",
							)
						})
					})

					when("image metadata has a mirror", func() {
						it.Before(func() {
							// clean up existing mirror first to avoid leaking images
							imageManager.CleanupImages(runImageMirror)

							buildRunImage(runImageMirror, "mirror-after-1", "mirror-after-2")
						})

						it("selects the best mirror", func() {
							output := pack.RunSuccessfully("rebase", repoName, "--pull-policy", "never")

							assertOutput := assertions.NewOutputAssertionManager(t, output)
							assertOutput.ReportsSelectingRunImageMirror(runImageMirror)
							assertOutput.ReportsSuccessfulRebase(repoName)
							assertImage.RunsWithOutput(
								repoName,
								"mirror-after-1",
								"mirror-after-2",
							)
						})
					})
				})

				when("--publish", func() {
					it.Before(func() {
						assert.Succeeds(h.PushImage(dockerCli, repoName, registryConfig))
					})

					when("--run-image", func() {
						var runAfter string

						it.Before(func() {
							runAfter = registryConfig.RepoName("run-after/" + h.RandString(10))
							buildRunImage(runAfter, "contents-after-1", "contents-after-2")
							assert.Succeeds(h.PushImage(dockerCli, runAfter, registryConfig))
						})

						it.After(func() {
							imageManager.CleanupImages(runAfter)
						})

						it("uses provided run image", func() {
							output := pack.RunSuccessfully("rebase", repoName, "--publish", "--run-image", runAfter)

							assertions.NewOutputAssertionManager(t, output).ReportsSuccessfulRebase(repoName)
							assertImage.CanBePulledFromRegistry(repoName)
							assertImage.RunsWithOutput(
								repoName,
								"contents-after-1",
								"contents-after-2",
							)
						})
					})
				})
			})
		})
	})
}

func buildModulesDir(bpAPIVersion string) string {
	return filepath.Join("testdata", "mock_buildpacks", bpAPIVersion)
}

func createComplexBuilder(t *testing.T,
	assert h.AssertionManager,
	pack *invoke.PackInvoker,
	lifecycle config.LifecycleAsset,
	buildpackManager buildpacks.BuildModuleManager,
	runImageMirror string,
) (string, error) {

	t.Log("creating complex builder image...")

	// CREATE TEMP WORKING DIR
	tmpDir, err := os.MkdirTemp("", "create-complex-test-builder")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	// ARCHIVE BUILDPACKS
	builderBuildpacks := []buildpacks.TestBuildModule{
		buildpacks.BpNoop,
		buildpacks.BpNoop2,
		buildpacks.BpOtherStack,
		buildpacks.BpReadEnv,
	}

	templateMapping := map[string]interface{}{
		"run_image_mirror": runImageMirror,
	}

	packageImageName := registryConfig.RepoName("nested-level-1-buildpack-" + h.RandString(8))
	nestedLevelTwoBuildpackName := registryConfig.RepoName("nested-level-2-buildpack-" + h.RandString(8))
	simpleLayersBuildpackName := registryConfig.RepoName("simple-layers-buildpack-" + h.RandString(8))
	simpleLayersBuildpackDifferentShaName := registryConfig.RepoName("simple-layers-buildpack-different-name-" + h.RandString(8))

	templateMapping["package_id"] = "simple/nested-level-1"
	templateMapping["package_image_name"] = packageImageName
	templateMapping["nested_level_1_buildpack"] = packageImageName
	templateMapping["nested_level_2_buildpack"] = nestedLevelTwoBuildpackName
	templateMapping["simple_layers_buildpack"] = simpleLayersBuildpackName
	templateMapping["simple_layers_buildpack_different_sha"] = simpleLayersBuildpackDifferentShaName

	fixtureManager := pack.FixtureManager()

	nestedLevelOneConfigFile, err := os.CreateTemp(tmpDir, "nested-level-1-package.toml")
	assert.Nil(err)
	fixtureManager.TemplateFixtureToFile(
		"nested-level-1-buildpack_package.toml",
		nestedLevelOneConfigFile,
		templateMapping,
	)
	err = nestedLevelOneConfigFile.Close()
	assert.Nil(err)

	nestedLevelTwoConfigFile, err := os.CreateTemp(tmpDir, "nested-level-2-package.toml")
	assert.Nil(err)
	fixtureManager.TemplateFixtureToFile(
		"nested-level-2-buildpack_package.toml",
		nestedLevelTwoConfigFile,
		templateMapping,
	)

	err = nestedLevelTwoConfigFile.Close()
	assert.Nil(err)

	packageImageBuildpack := buildpacks.NewPackageImage(
		t,
		pack,
		packageImageName,
		nestedLevelOneConfigFile.Name(),
		buildpacks.WithRequiredBuildpacks(
			buildpacks.BpNestedLevelOne,
			buildpacks.NewPackageImage(
				t,
				pack,
				nestedLevelTwoBuildpackName,
				nestedLevelTwoConfigFile.Name(),
				buildpacks.WithRequiredBuildpacks(
					buildpacks.BpNestedLevelTwo,
					buildpacks.NewPackageImage(
						t,
						pack,
						simpleLayersBuildpackName,
						fixtureManager.FixtureLocation("simple-layers-buildpack_package.toml"),
						buildpacks.WithRequiredBuildpacks(buildpacks.BpSimpleLayers),
					),
				),
			),
		),
	)

	simpleLayersDifferentShaBuildpack := buildpacks.NewPackageImage(
		t,
		pack,
		simpleLayersBuildpackDifferentShaName,
		fixtureManager.FixtureLocation("simple-layers-buildpack-different-sha_package.toml"),
		buildpacks.WithRequiredBuildpacks(buildpacks.BpSimpleLayersDifferentSha),
	)

	defer imageManager.CleanupImages(packageImageName, nestedLevelTwoBuildpackName, simpleLayersBuildpackName, simpleLayersBuildpackDifferentShaName)

	builderBuildpacks = append(
		builderBuildpacks,
		packageImageBuildpack,
		simpleLayersDifferentShaBuildpack,
	)

	buildpackManager.PrepareBuildModules(tmpDir, builderBuildpacks...)

	// ADD lifecycle
	if lifecycle.HasLocation() {
		lifecycleURI := lifecycle.EscapedPath()
		t.Logf("adding lifecycle path '%s' to builder config", lifecycleURI)
		templateMapping["lifecycle_uri"] = lifecycleURI
	} else {
		lifecycleVersion := lifecycle.Version()
		t.Logf("adding lifecycle version '%s' to builder config", lifecycleVersion)
		templateMapping["lifecycle_version"] = lifecycleVersion
	}

	// RENDER builder.toml
	builderConfigFile, err := os.CreateTemp(tmpDir, "nested_builder.toml")
	if err != nil {
		return "", err
	}

	pack.FixtureManager().TemplateFixtureToFile("nested_builder.toml", builderConfigFile, templateMapping)

	err = builderConfigFile.Close()
	if err != nil {
		return "", err
	}

	// NAME BUILDER
	bldr := registryConfig.RepoName("test/builder-" + h.RandString(10))

	// CREATE BUILDER
	output := pack.RunSuccessfully(
		"builder", "create", bldr,
		"-c", builderConfigFile.Name(),
		"--no-color",
	)

	assert.Contains(output, fmt.Sprintf("Successfully created builder image '%s'", bldr))
	assert.Succeeds(h.PushImage(dockerCli, bldr, registryConfig))

	return bldr, nil
}

func createBuilder(
	t *testing.T,
	assert h.AssertionManager,
	pack *invoke.PackInvoker,
	lifecycle config.LifecycleAsset,
	buildpackManager buildpacks.BuildModuleManager,
	runImageMirror string,
) (string, error) {
	t.Log("creating builder image...")

	// CREATE TEMP WORKING DIR
	tmpDir, err := os.MkdirTemp("", "create-test-builder")
	assert.Nil(err)
	defer os.RemoveAll(tmpDir)

	templateMapping := map[string]interface{}{
		"run_image_mirror": runImageMirror,
	}

	// ARCHIVE BUILDPACKS
	builderBuildpacks := []buildpacks.TestBuildModule{
		buildpacks.BpNoop,
		buildpacks.BpNoop2,
		buildpacks.BpOtherStack,
		buildpacks.BpReadEnv,
	}

	packageTomlPath := generatePackageTomlWithOS(t, assert, pack, tmpDir, "package.toml", imageManager.HostOS())
	packageImageName := registryConfig.RepoName("simple-layers-package-image-buildpack-" + h.RandString(8))

	packageImageBuildpack := buildpacks.NewPackageImage(
		t,
		pack,
		packageImageName,
		packageTomlPath,
		buildpacks.WithRequiredBuildpacks(buildpacks.BpSimpleLayers),
	)

	defer imageManager.CleanupImages(packageImageName)

	builderBuildpacks = append(builderBuildpacks, packageImageBuildpack)

	templateMapping["package_image_name"] = packageImageName
	templateMapping["package_id"] = "simple/layers"

	buildpackManager.PrepareBuildModules(tmpDir, builderBuildpacks...)

	// ADD lifecycle
	var lifecycleURI string
	var lifecycleVersion string
	if lifecycle.HasLocation() {
		lifecycleURI = lifecycle.EscapedPath()
		t.Logf("adding lifecycle path '%s' to builder config", lifecycleURI)
		templateMapping["lifecycle_uri"] = lifecycleURI
	} else {
		lifecycleVersion = lifecycle.Version()
		t.Logf("adding lifecycle version '%s' to builder config", lifecycleVersion)
		templateMapping["lifecycle_version"] = lifecycleVersion
	}

	// RENDER builder.toml
	configFileName := "builder.toml"

	builderConfigFile, err := os.CreateTemp(tmpDir, "builder.toml")
	assert.Nil(err)

	pack.FixtureManager().TemplateFixtureToFile(
		configFileName,
		builderConfigFile,
		templateMapping,
	)

	err = builderConfigFile.Close()
	assert.Nil(err)

	// NAME BUILDER
	bldr := registryConfig.RepoName("test/builder-" + h.RandString(10))

	// CREATE BUILDER
	output := pack.RunSuccessfully(
		"builder", "create", bldr,
		"-c", builderConfigFile.Name(),
		"--no-color",
	)

	assert.Contains(output, fmt.Sprintf("Successfully created builder image '%s'", bldr))
	assert.Succeeds(h.PushImage(dockerCli, bldr, registryConfig))

	return bldr, nil
}

func createBuilderWithExtensions(
	t *testing.T,
	assert h.AssertionManager,
	pack *invoke.PackInvoker,
	lifecycle config.LifecycleAsset,
	buildpackManager buildpacks.BuildModuleManager,
	runImageMirror string,
) (string, error) {
	t.Log("creating builder image with extensions...")

	// CREATE TEMP WORKING DIR
	tmpDir, err := os.MkdirTemp("", "create-test-builder-extensions")
	assert.Nil(err)
	defer os.RemoveAll(tmpDir)

	templateMapping := map[string]interface{}{
		"run_image_mirror": runImageMirror,
	}

	// BUILDPACKS
	builderBuildpacks := []buildpacks.TestBuildModule{
		buildpacks.BpReadEnv,            // archive buildpack
		buildpacks.BpFolderSimpleLayers, // folder buildpack
	}
	buildpackManager.PrepareBuildModules(tmpDir, builderBuildpacks...)

	// EXTENSIONS
	builderExtensions := []buildpacks.TestBuildModule{
		buildpacks.ExtReadEnv,            // archive extension
		buildpacks.ExtFolderSimpleLayers, // folder extension
	}
	buildpackManager.PrepareBuildModules(tmpDir, builderExtensions...)

	// ADD lifecycle
	var lifecycleURI string
	var lifecycleVersion string
	if lifecycle.HasLocation() {
		lifecycleURI = lifecycle.EscapedPath()
		t.Logf("adding lifecycle path '%s' to builder config", lifecycleURI)
		templateMapping["lifecycle_uri"] = lifecycleURI
	} else {
		lifecycleVersion = lifecycle.Version()
		t.Logf("adding lifecycle version '%s' to builder config", lifecycleVersion)
		templateMapping["lifecycle_version"] = lifecycleVersion
	}

	// RENDER builder.toml
	configFileName := "builder_extensions.toml"

	builderConfigFile, err := os.CreateTemp(tmpDir, "builder.toml")
	assert.Nil(err)

	pack.FixtureManager().TemplateFixtureToFile(
		configFileName,
		builderConfigFile,
		templateMapping,
	)

	err = builderConfigFile.Close()
	assert.Nil(err)

	// NAME BUILDER
	bldr := registryConfig.RepoName("test/builder-" + h.RandString(10))

	// SET EXPERIMENTAL
	pack.JustRunSuccessfully("config", "experimental", "true")

	// CREATE BUILDER
	output := pack.RunSuccessfully(
		"builder", "create", bldr,
		"-c", builderConfigFile.Name(),
		"--no-color",
	)

	assert.Contains(output, fmt.Sprintf("Successfully created builder image '%s'", bldr))
	assert.Succeeds(h.PushImage(dockerCli, bldr, registryConfig))

	return bldr, nil
}

func generatePackageTomlWithOS(
	t *testing.T,
	assert h.AssertionManager,
	pack *invoke.PackInvoker,
	tmpDir string,
	fixtureName string,
	platform_os string,
) string {
	t.Helper()

	packageTomlFile, err := os.CreateTemp(tmpDir, "package-*.toml")
	assert.Nil(err)

	pack.FixtureManager().TemplateFixtureToFile(
		fixtureName,
		packageTomlFile,
		map[string]interface{}{
			"OS": platform_os,
		},
	)

	assert.Nil(packageTomlFile.Close())

	return packageTomlFile.Name()
}

func createStack(t *testing.T, dockerCli client.CommonAPIClient, runImageMirror string) error {
	t.Helper()
	t.Log("creating stack images...")

	stackBaseDir := filepath.Join("testdata", "mock_stack", imageManager.HostOS())

	if err := createStackImage(dockerCli, runImage, filepath.Join(stackBaseDir, "run")); err != nil {
		return err
	}
	if err := createStackImage(dockerCli, buildImage, filepath.Join(stackBaseDir, "build")); err != nil {
		return err
	}

	imageManager.TagImage(runImage, runImageMirror)
	if err := h.PushImage(dockerCli, runImageMirror, registryConfig); err != nil {
		return err
	}

	return nil
}

func createStackImage(dockerCli client.CommonAPIClient, repoName string, dir string) error {
	defaultFilterFunc := func(file string) bool { return true }

	ctx := context.Background()
	buildContext := archive.ReadDirAsTar(dir, "/", 0, 0, -1, true, false, defaultFilterFunc)

	return h.CheckImageBuildResult(dockerCli.ImageBuild(ctx, buildContext, dockertypes.ImageBuildOptions{
		Tags:        []string{repoName},
		Remove:      true,
		ForceRemove: true,
	}))
}

// taskKey creates a key from the prefix and all arguments to be unique
func taskKey(prefix string, args ...string) string {
	hash := sha256.New()
	for _, v := range args {
		hash.Write([]byte(v))
	}
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(hash.Sum(nil)))
}

type compareFormat struct {
	extension   string
	compareFunc func(string, string)
	outputArg   string
}

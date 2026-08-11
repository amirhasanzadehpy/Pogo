const { chmodSync, copyFileSync, mkdirSync, rmSync } = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const targets = [
  ['linux', 'amd64'],
  ['linux', 'arm64'],
  ['darwin', 'amd64'],
  ['darwin', 'arm64'],
  ['windows', 'amd64'],
  ['windows', 'arm64'],
];

const extensionRoot = path.resolve(__dirname, '..');
const repositoryRoot = path.resolve(extensionRoot, '..', '..');
const outputRoot = path.join(extensionRoot, 'bin');
const sourceArgument = process.argv.find((argument) => argument.startsWith('--source='));
const sourceRoot =
  sourceArgument === undefined
    ? undefined
    : path.resolve(extensionRoot, sourceArgument.slice('--source='.length));

rmSync(outputRoot, { recursive: true, force: true });

for (const [goos, goarch] of targets) {
  const targetName = `${goos}-${goarch}`;
  const executableName = goos === 'windows' ? 'pogo.exe' : 'pogo';
  const outputDirectory = path.join(outputRoot, targetName);
  const outputPath = path.join(outputDirectory, executableName);
  mkdirSync(outputDirectory, { recursive: true });

  if (sourceRoot !== undefined) {
    copyFileSync(path.join(sourceRoot, targetName, executableName), outputPath);
  } else {
    const result = spawnSync(
      'go',
      [
        'build',
        '-trimpath',
        '-tags=grammar_subset,grammar_subset_python',
        '-ldflags=-s -w',
        '-o',
        outputPath,
        './cmd/pogo',
      ],
      {
        cwd: repositoryRoot,
        env: { ...process.env, CGO_ENABLED: '0', GOOS: goos, GOARCH: goarch },
        stdio: 'inherit',
      },
    );
    if (result.error !== undefined) {
      throw result.error;
    }
    if (result.status !== 0) {
      process.exit(result.status ?? 1);
    }
  }

  if (goos !== 'windows') {
    chmodSync(outputPath, 0o755);
  }
}

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');
const { buildSync } = require('esbuild');

// Exercise the real client construction without launching an extension host.
const sourceDirectory = path.resolve(__dirname, '../src');
const bundled = buildSync({
  stdin: {
    contents: fs.readFileSync(path.join(sourceDirectory, 'extension.ts'), 'utf8') +
      '\nexport { createClient };\n',
    resolveDir: sourceDirectory,
    loader: 'ts',
  },
  bundle: true,
  platform: 'node',
  format: 'cjs',
  external: ['vscode', 'vscode-languageclient/node'],
  write: false,
}).outputFiles[0].text;

function loadClientFactory(settings, folder) {
  const module = { exports: {} };
  vm.runInNewContext(bundled, {
    module,
    exports: module.exports,
    console,
    process,
    require(name) {
      if (name === 'vscode') {
        return {
          workspace: {
            getConfiguration(section, resource) {
              assert.equal(section, 'pogo');
              assert.equal(resource, folder?.uri);
              return { get: (key, fallback) => settings[key] ?? fallback };
            },
          },
          extensions: { getExtension: () => undefined },
        };
      }
      if (name === 'vscode-languageclient/node') {
        return {
          LanguageClient: class {
            constructor(id, name, serverOptions, clientOptions) {
              Object.assign(this, { id, name, serverOptions, clientOptions });
            }
          },
        };
      }
      return require(name);
    },
  });
  return module.exports.createClient;
}

const workspace = path.resolve('test-workspace');
const folder = {
  name: 'workspace',
  uri: { fsPath: workspace, toString: () => 'file:///test-workspace' },
};

for (const [name, configured, expected] of [
  ['default', '', workspace],
  ['whitespace default', '   ', workspace],
  ['relative nested root', ' project_root ', path.join(workspace, 'project_root')],
  ['absolute root', path.resolve('other-project'), path.resolve('other-project')],
]) {
  test(`project root: ${name}`, async () => {
    const createClient = loadClientFactory({
      projectRoot: configured,
      pythonPath: path.join('.venv', 'bin', 'python'),
      envFile: '.env.pogo',
      settingsModule: 'project_settings.settings',
    }, folder);
    const client = await createClient({ key: folder.uri.toString(), folder }, 'pogo');
    const options = client.clientOptions;
    const djangoOrm = options.initializationOptions.djangoOrm;
    assert.equal(djangoOrm.projectRoot, expected);
    assert.equal(djangoOrm.pythonPath, path.join(workspace, '.venv', 'bin', 'python'));
    assert.equal(djangoOrm.environmentFile, path.join(workspace, '.env.pogo'));
    assert.equal(djangoOrm.settingsModule, 'project_settings.settings');
    assert.equal(options.workspaceFolder, folder);
    assert.equal(options.documentSelector[0].pattern.baseUri, folder.uri.toString());
    assert.equal(client.serverOptions.options.cwd, workspace);
  });
}

test('standalone windows have no Django initialization options', async () => {
  const createClient = loadClientFactory({ projectRoot: 'project_root' });
  const client = await createClient({ key: 'standalone' }, 'pogo');
  assert.equal(client.clientOptions.initializationOptions, undefined);
});

test('each workspace resolves its own relative project root', async () => {
  for (const name of ['first-workspace', 'second-workspace']) {
    const root = path.resolve(name);
    const owningFolder = {
      name,
      uri: { fsPath: root, toString: () => `file:///${name}` },
    };
    const createClient = loadClientFactory({ projectRoot: 'backend' }, owningFolder);
    const client = await createClient({
      key: owningFolder.uri.toString(),
      folder: owningFolder,
    }, 'pogo');
    assert.equal(client.clientOptions.initializationOptions.djangoOrm.projectRoot,
      path.join(root, 'backend'));
    assert.equal(client.clientOptions.workspaceFolder, owningFolder);
  }
});

test('project-root setting is exposed per workspace', () => {
  const manifest = require('../package.json');
  const setting = manifest.contributes.configuration.properties['pogo.projectRoot'];
  assert.equal(setting.type, 'string');
  assert.equal(setting.default, '');
  assert.equal(setting.scope, 'resource');
});

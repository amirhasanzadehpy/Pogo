import { constants } from 'node:fs';
import { access, stat } from 'node:fs/promises';
import * as path from 'node:path';
import * as vscode from 'vscode';
import {
  LanguageClient,
  type LanguageClientOptions,
  type ServerOptions,
} from 'vscode-languageclient/node';

const executableName = 'django-orm-lsp';
const executableSetting = 'pogo.executablePath';
const releasesURL = vscode.Uri.parse(
  'https://github.com/amirhasanzadehpy/Pogo/releases',
);
const standaloneKey = 'standalone';

interface ClientTarget {
  readonly key: string;
  readonly folder?: vscode.WorkspaceFolder;
}

const clients = new Map<string, LanguageClient>();
let lifecycle = Promise.resolve();
let shuttingDown = false;
let notificationVisible = false;

export async function activate(context: vscode.ExtensionContext): Promise<void> {
  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((event) => {
      if (event.affectsConfiguration('pogo')) {
        scheduleReconcile(true);
      }
    }),
    vscode.workspace.onDidChangeWorkspaceFolders(() => {
      // Restart retained clients so open documents move cleanly when nested
      // workspace-folder ownership changes.
      scheduleReconcile(true);
    }),
  );

  scheduleReconcile(false);
  await lifecycle;
}

export async function deactivate(): Promise<void> {
  shuttingDown = true;
  await lifecycle.catch(logLifecycleFailure);
  await disposeClients([...clients.keys()]);
}

function scheduleReconcile(restartAll: boolean): void {
  if (shuttingDown) {
    return;
  }

  lifecycle = lifecycle
    .catch(logLifecycleFailure)
    .then(() => reconcileClients(restartAll));
}

function logLifecycleFailure(error: unknown): void {
  console.error('Pogo client lifecycle failed:', error);
}

async function reconcileClients(restartAll: boolean): Promise<void> {
  if (shuttingDown) {
    return;
  }

  const targets = currentTargets();
  const desiredKeys = new Set(targets.map((target) => target.key));
  const obsoleteKeys = [...clients.keys()].filter(
    (key) => restartAll || !desiredKeys.has(key),
  );
  await disposeClients(obsoleteKeys);

  for (const target of targets) {
    if (shuttingDown || clients.has(target.key)) {
      continue;
    }

    const executable = await resolveExecutable(target.folder);
    if (executable === undefined) {
      notifyMissingExecutable(target.folder);
      continue;
    }

    const client = createClient(target, executable);
    clients.set(target.key, client);
    try {
      await client.start();
    } catch (error: unknown) {
      clients.delete(target.key);
      await client.dispose().catch(() => undefined);
      console.error(`Failed to start Pogo client ${target.key}:`, error);
      void vscode.window.showErrorMessage(
        `Pogo could not start for ${target.folder?.name ?? 'this window'}. ` +
          'Open the Pogo output channel for details.',
      );
    }
  }
}

function currentTargets(): ClientTarget[] {
  const folders = (vscode.workspace.workspaceFolders ?? []).filter(
    (folder) => folder.uri.scheme === 'file',
  );
  if (folders.length === 0) {
    return [{ key: standaloneKey }];
  }
  return folders.map((folder) => ({
    key: folder.uri.toString(),
    folder,
  }));
}

function createClient(target: ClientTarget, executable: string): LanguageClient {
  const configuration = vscode.workspace.getConfiguration(
    'pogo',
    target.folder?.uri,
  );
  const pythonPath = resolveWorkspacePath(
    configuration.get<string>('pythonPath', '').trim(),
    target.folder,
  );
  const settingsModule = configuration
    .get<string>('settingsModule', '')
    .trim();
  const djangoOrm =
    target.folder === undefined
      ? undefined
      : {
          projectRoot: target.folder.uri.fsPath,
          ...(pythonPath === '' ? {} : { pythonPath }),
          ...(settingsModule === '' ? {} : { settingsModule }),
        };
  const serverOptions: ServerOptions = {
    command: executable,
    options:
      target.folder === undefined
        ? undefined
        : { cwd: target.folder.uri.fsPath },
  };
  const clientOptions: LanguageClientOptions = {
    documentSelector: [
      target.folder === undefined
        ? { language: 'python', scheme: 'file' }
        : {
            language: 'python',
            scheme: 'file',
            pattern: {
              baseUri: target.folder.uri.toString(),
              pattern: '**/*',
            },
          },
    ],
    workspaceFolder: target.folder,
    initializationOptions:
      djangoOrm === undefined ? undefined : { djangoOrm },
    diagnosticCollectionName: 'pogo',
    outputChannelName:
      target.folder === undefined ? 'Pogo' : `Pogo (${target.folder.name})`,
    middleware: {
      didOpen: (document, next) => {
        if (ownsDocument(target, document.uri)) {
          return next(document);
        }
        return Promise.resolve();
      },
      didChange: (event, next) => {
        if (ownsDocument(target, event.document.uri)) {
          return next(event);
        }
        return Promise.resolve();
      },
      didSave: (document, next) => {
        if (ownsDocument(target, document.uri)) {
          return next(document);
        }
        return Promise.resolve();
      },
      didClose: (document, next) => {
        if (ownsDocument(target, document.uri)) {
          return next(document);
        }
        return Promise.resolve();
      },
      provideCompletionItem: (document, position, context, token, next) =>
        ownsDocument(target, document.uri)
          ? next(document, position, context, token)
          : null,
      provideHover: (document, position, token, next) =>
        ownsDocument(target, document.uri)
          ? next(document, position, token)
          : null,
      provideSignatureHelp: (document, position, context, token, next) =>
        ownsDocument(target, document.uri)
          ? next(document, position, context, token)
          : null,
      provideDefinition: (document, position, token, next) =>
        ownsDocument(target, document.uri)
          ? next(document, position, token)
          : null,
      provideDocumentLinks: (document, token, next) =>
        ownsDocument(target, document.uri)
          ? next(document, token)
          : null,
      handleDiagnostics: (uri, diagnostics, next) => {
        if (ownsDocument(target, uri)) {
          return next(uri, diagnostics);
        }
      },
    },
  };
  return new LanguageClient(
    target.folder === undefined ? 'pogo' : `pogo:${target.key}`,
    'Pogo',
    serverOptions,
    clientOptions,
  );
}

function ownsDocument(target: ClientTarget, uri: vscode.Uri): boolean {
  const folder = vscode.workspace.getWorkspaceFolder(uri);
  if (target.folder === undefined) {
    return folder === undefined;
  }
  return folder?.uri.toString() === target.key;
}

function resolveWorkspacePath(
  configured: string,
  folder: vscode.WorkspaceFolder | undefined,
): string {
  if (configured === '' || path.isAbsolute(configured) || folder === undefined) {
    return configured;
  }
  return path.resolve(folder.uri.fsPath, configured);
}

async function resolveExecutable(
  folder: vscode.WorkspaceFolder | undefined,
): Promise<string | undefined> {
  const configured = vscode.workspace
    .getConfiguration('pogo', folder?.uri)
    .get<string>('executablePath', '')
    .trim();
  if (configured !== '') {
    if (!path.isAbsolute(configured) && folder === undefined) {
      return undefined;
    }
    const candidate = path.isAbsolute(configured)
      ? path.normalize(configured)
      : path.resolve(folder!.uri.fsPath, configured);
    return (await isExecutableFile(candidate)) ? candidate : undefined;
  }
  return findExecutableOnPath(folder?.uri.fsPath ?? process.cwd());
}

async function findExecutableOnPath(
  workingDirectory: string,
): Promise<string | undefined> {
  const pathValue = environmentValue('PATH');
  if (pathValue === undefined) {
    return undefined;
  }
  const names =
    process.platform === 'win32'
      ? [`${executableName}.exe`, `${executableName}.com`, executableName]
      : [executableName];
  const seen = new Set<string>();

  for (const rawEntry of pathValue.split(path.delimiter)) {
    const entry = unquotePathEntry(rawEntry);
    const directory =
      entry === ''
        ? workingDirectory
        : path.isAbsolute(entry)
          ? entry
          : path.resolve(workingDirectory, entry);
    for (const name of names) {
      const candidate = path.join(directory, name);
      const comparisonKey =
        process.platform === 'win32' ? candidate.toLowerCase() : candidate;
      if (seen.has(comparisonKey)) {
        continue;
      }
      seen.add(comparisonKey);
      if (await isExecutableFile(candidate)) {
        return candidate;
      }
    }
  }
  return undefined;
}

async function isExecutableFile(candidate: string): Promise<boolean> {
  try {
    if (!(await stat(candidate)).isFile()) {
      return false;
    }
    await access(
      candidate,
      process.platform === 'win32' ? constants.F_OK : constants.X_OK,
    );
    return true;
  } catch {
    return false;
  }
}

function environmentValue(name: string): string | undefined {
  if (process.platform !== 'win32') {
    return process.env[name];
  }
  const key = Object.keys(process.env).find(
    (candidate) => candidate.toUpperCase() === name.toUpperCase(),
  );
  return key === undefined ? undefined : process.env[key];
}

function unquotePathEntry(value: string): string {
  const trimmed = value.trim();
  if (
    trimmed.length >= 2 &&
    trimmed.startsWith('"') &&
    trimmed.endsWith('"')
  ) {
    return trimmed.slice(1, -1);
  }
  return trimmed;
}

function notifyMissingExecutable(folder: vscode.WorkspaceFolder | undefined): void {
  if (notificationVisible) {
    return;
  }
  notificationVisible = true;
  const location = folder === undefined ? 'this window' : folder.name;
  void vscode.window
    .showErrorMessage(
      `Pogo could not find '${executableName}' for ${location}. ` +
        `Download the binary and add it to PATH, or set '${executableSetting}'.`,
      'Download Pogo',
      'Open Settings',
    )
    .then(async (selection) => {
      notificationVisible = false;
      if (selection === 'Download Pogo') {
        await vscode.env.openExternal(releasesURL);
      } else if (selection === 'Open Settings') {
        await vscode.commands.executeCommand(
          'workbench.action.openSettings',
          executableSetting,
        );
      }
    });
}

async function disposeClients(keys: readonly string[]): Promise<void> {
  await Promise.all(
    keys.map(async (key) => {
      const client = clients.get(key);
      if (client === undefined) {
        return;
      }
      clients.delete(key);
      try {
        await client.dispose();
      } catch (error: unknown) {
        console.error(`Failed to stop Pogo client ${key}:`, error);
      }
    }),
  );
}

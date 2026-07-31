# Pogo for VS Code

Pogo adds Django ORM completion, hover information, signature help,
diagnostics, definitions, and document links to Python files.

Install `pogo` on the extension host's `PATH`, then open a trusted
Django workspace. For a local source build, set **Pogo: Executable Path** to
the absolute path to `build/pogo`; relative paths are resolved from
each workspace folder.
The **Pogo: Python Path** and **Pogo: Settings Module** settings override
automatic Django environment discovery when a project is ambiguous. **Pogo:
Env File** loads project dotenv values, and **Pogo: Environment** can override
or remove inherited variables for Django startup.
Keep secrets in an ignored, permission-restricted dotenv file rather than
plaintext workspace settings.

Pogo starts Django to load the runtime model schema. Only use it with projects
you trust. Full binary installation and troubleshooting instructions are in
the [Pogo product manual](https://github.com/amirhasanzadehpy/Pogo#readme).

/*
 * JSON schemas for well-known files (validation, hovers and completions in the JSON language service). Bundled and
 * deliberately small: the editor works offline and never fetches a schema.
 */

interface SchemaEntry {
  uri: string
  fileMatch: string[]
  schema: object
}

const deps = { type: 'object', additionalProperties: { type: 'string' }, description: 'Package name → version range' }

const packageJson = {
  type: 'object',
  properties: {
    name: { type: 'string', maxLength: 214, pattern: '^(?:@[a-z0-9-*~][a-z0-9-*._~]*/)?[a-z0-9-~][a-z0-9-._~]*$', description: 'The package name' },
    version: { type: 'string', description: 'Semantic version (x.y.z)' },
    description: { type: 'string' },
    keywords: { type: 'array', items: { type: 'string' } },
    homepage: { type: 'string' },
    license: { type: 'string' },
    author: { oneOf: [{ type: 'string' }, { type: 'object', properties: { name: { type: 'string' }, email: { type: 'string' }, url: { type: 'string' } } }] },
    private: { type: 'boolean', description: 'Refuse to publish this package' },
    type: { enum: ['module', 'commonjs'], description: 'How .js files are interpreted' },
    main: { type: 'string' },
    module: { type: 'string' },
    types: { type: 'string' },
    bin: { oneOf: [{ type: 'string' }, { type: 'object', additionalProperties: { type: 'string' } }] },
    files: { type: 'array', items: { type: 'string' } },
    exports: {},
    scripts: { type: 'object', additionalProperties: { type: 'string' }, description: 'Commands run with `npm run <name>`' },
    engines: { type: 'object', additionalProperties: { type: 'string' } },
    repository: { oneOf: [{ type: 'string' }, { type: 'object', properties: { type: { type: 'string' }, url: { type: 'string' } } }] },
    dependencies: deps,
    devDependencies: deps,
    peerDependencies: deps,
    optionalDependencies: deps,
    workspaces: { oneOf: [{ type: 'array', items: { type: 'string' } }, { type: 'object' }] },
  },
}

const tsconfig = {
  type: 'object',
  properties: {
    extends: { oneOf: [{ type: 'string' }, { type: 'array', items: { type: 'string' } }] },
    files: { type: 'array', items: { type: 'string' } },
    include: { type: 'array', items: { type: 'string' } },
    exclude: { type: 'array', items: { type: 'string' } },
    references: { type: 'array', items: { type: 'object', properties: { path: { type: 'string' } } } },
    compilerOptions: {
      type: 'object',
      properties: {
        target: { type: 'string', description: 'ECMAScript target version (ES2022, ESNext, …)' },
        module: { type: 'string', description: 'Module system (ESNext, NodeNext, CommonJS, …)' },
        moduleResolution: { type: 'string', enum: ['node', 'node10', 'node16', 'nodenext', 'bundler', 'classic', 'Node', 'Node10', 'Node16', 'NodeNext', 'Bundler', 'Classic'] },
        lib: { type: 'array', items: { type: 'string' } },
        jsx: { type: 'string', enum: ['preserve', 'react', 'react-jsx', 'react-jsxdev', 'react-native'] },
        strict: { type: 'boolean' },
        noEmit: { type: 'boolean' },
        outDir: { type: 'string' },
        rootDir: { type: 'string' },
        baseUrl: { type: 'string' },
        paths: { type: 'object', additionalProperties: { type: 'array', items: { type: 'string' } } },
        types: { type: 'array', items: { type: 'string' } },
        allowJs: { type: 'boolean' },
        checkJs: { type: 'boolean' },
        declaration: { type: 'boolean' },
        sourceMap: { type: 'boolean' },
        esModuleInterop: { type: 'boolean' },
        skipLibCheck: { type: 'boolean' },
        isolatedModules: { type: 'boolean' },
        resolveJsonModule: { type: 'boolean' },
        noUnusedLocals: { type: 'boolean' },
        noUnusedParameters: { type: 'boolean' },
      },
    },
  },
}

export const JSON_SCHEMAS: SchemaEntry[] = [
  { uri: 'astraterm://schemas/package.json', fileMatch: ['**/package.json'], schema: packageJson },
  { uri: 'astraterm://schemas/tsconfig.json', fileMatch: ['**/tsconfig.json', '**/tsconfig.*.json', '**/jsconfig.json'], schema: tsconfig },
]

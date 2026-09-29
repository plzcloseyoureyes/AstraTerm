// Remote path helpers (paths.ts): normalisation, Windows drive roots, location-bar input, names, shell quoting.
import assert from 'node:assert/strict'
import test from 'node:test'
import { basename, breadcrumbs, dirname, isInside, isRoot, joinPath, normalizePath, resolveInput, shellQuote, splitName, uniqueName, validateName } from '../paths'

test('normalizePath / joinPath / dirname / basename', () => {
  assert.equal(normalizePath('/a//b/./c/../d/'), '/a/b/d')
  assert.equal(normalizePath('/../..'), '/')
  assert.equal(normalizePath('C:\\Users\\x\\..\\y'), 'C:/Users/y')
  assert.equal(normalizePath('C:'), 'C:/')
  assert.equal(normalizePath(''), '/')
  assert.equal(joinPath('/a/b', '../c'), '/a/c')
  assert.equal(joinPath('/a', '/abs'), '/abs')
  assert.equal(dirname('/a/b'), '/a')
  assert.equal(dirname('/a'), '/')
  assert.equal(dirname('/'), '/')
  assert.equal(dirname('C:/x'), 'C:/')
  assert.equal(basename('/a/b.txt'), 'b.txt')
  assert.equal(basename('/'), '/')
  assert.equal(isRoot('/'), true)
  assert.equal(isRoot('D:/'), true)
  assert.equal(isRoot('/tmp'), false)
})

test('isInside does not match name prefixes', () => {
  assert.equal(isInside('/a/b/c', '/a/b'), true)
  assert.equal(isInside('/a/b', '/a/b'), true)
  assert.equal(isInside('/a/bc', '/a/b'), false)
  assert.equal(isInside('/x', '/'), true)
})

test('resolveInput: ~, relative, absolute', () => {
  assert.equal(resolveInput('~', '/cur', '/home/u'), '/home/u')
  assert.equal(resolveInput('~/x/../y', '/cur', '/home/u'), '/home/u/y')
  assert.equal(resolveInput('../etc', '/var/log', '/h'), '/var/etc')
  assert.equal(resolveInput('  /opt  ', '/var', '/h'), '/opt')
  assert.equal(resolveInput('', '/var', '/h'), '/var')
})

test('breadcrumbs', () => {
  assert.deepEqual(breadcrumbs('/a/b'), [
    { name: '/', path: '/' },
    { name: 'a', path: '/a' },
    { name: 'b', path: '/a/b' },
  ])
  assert.deepEqual(
    breadcrumbs('C:/Users').map((c) => c.path),
    ['C:/', 'C:/Users'],
  )
})

test('names: split, unique, validate', () => {
  assert.deepEqual(splitName('a.tar.gz'), { stem: 'a', ext: '.tar.gz' })
  assert.deepEqual(splitName('.bashrc'), { stem: '.bashrc', ext: '' })
  assert.equal(uniqueName('r.pdf', new Set(['r.pdf', 'r (1).pdf'])), 'r (2).pdf')
  assert.equal(uniqueName('new', new Set()), 'new')
  assert.equal(validateName(' '), 'Enter a name')
  assert.equal(validateName('..'), 'This name is reserved')
  assert.equal(validateName('a/b'), 'A name cannot contain “/”')
  assert.equal(validateName('ok name.txt'), null)
})

test('shellQuote survives quotes and shell metacharacters (cd sent to the terminal)', () => {
  assert.equal(shellQuote("/tmp/it's here"), `'/tmp/it'\\''s here'`)
  assert.equal(shellQuote('/x/$(rm -rf ~)`id`;&|'), `'/x/$(rm -rf ~)\`id\`;&|'`)
  assert.equal(shellQuote(''), `''`)
})

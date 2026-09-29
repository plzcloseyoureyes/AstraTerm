import assert from 'node:assert/strict'
import test from 'node:test'
import { essentialBinding } from '../keys'

test('essential bindings inside keyboard owners (Monaco)', () => {
  assert.equal(essentialBinding(undefined, '$mod+k'), false)
  assert.equal(essentialBinding(true, 'Control+Tab'), true)
  // the palette keeps ⇧⌘P but leaves ⌘K (Monaco chords) to the editor
  assert.equal(essentialBinding(['$mod+Shift+p'], '$mod+k'), false)
  assert.equal(essentialBinding(['$mod+Shift+p'], '$mod+Shift+p'), true)
})

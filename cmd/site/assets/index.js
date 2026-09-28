(function () {
  var input = document.getElementById('search');
  var cards = Array.prototype.slice.call(document.querySelectorAll('.card'));
  var count = document.getElementById('count');
  var empty = document.getElementById('empty');
  var total = cards.length;

  // Ignore spaces, dashes and punctuation so "bamdad khomar" matches "bamdad-khomar".
  function norm(s) {
    return s.toLowerCase().replace(/[\s\-_.,:;'"()]+/g, '');
  }
  cards.forEach(function (c) { c._key = norm(c.getAttribute('data-search')); });

  function apply() {
    var words = input.value.toLowerCase().split(/\s+/).map(norm).filter(Boolean);
    var shown = 0;
    cards.forEach(function (c) {
      var ok = words.every(function (w) { return c._key.indexOf(w) !== -1; });
      c.hidden = !ok;
      if (ok) shown++;
    });
    count.textContent = words.length ? shown + ' of ' + total + ' series' : total + ' series';
    empty.hidden = shown !== 0;

    var url = new URL(location.href);
    if (input.value) url.searchParams.set('q', input.value); else url.searchParams.delete('q');
    history.replaceState(null, '', url);
  }

  var q = new URL(location.href).searchParams.get('q');
  if (q) input.value = q;
  input.addEventListener('input', apply);
  apply();

  document.addEventListener('keydown', function (e) {
    if (e.key === '/' && document.activeElement !== input) {
      e.preventDefault();
      input.focus();
      input.select();
    } else if (e.key === 'Escape' && document.activeElement === input) {
      input.value = '';
      apply();
    }
  });
})();

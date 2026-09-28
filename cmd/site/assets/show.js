(function () {
  var player = document.getElementById('player');
  var wrap = document.getElementById('player-wrap');
  var nowPlaying = document.getElementById('now-playing');
  var prevBtn = document.getElementById('prev');
  var nextBtn = document.getElementById('next');
  var episodes = Array.prototype.slice.call(document.querySelectorAll('.episode:not([disabled])'));
  var storeKey = 'last:' + document.body.getAttribute('data-show');
  var current = -1;

  function el(tag, attrs) {
    var e = document.createElement(tag);
    for (var k in attrs) e.setAttribute(k, attrs[k]);
    return e;
  }

  function failed(src, msg) {
    player.textContent = '';
    var box = el('div', { class: 'player-empty' });
    var p = el('p', {});
    p.textContent = msg;
    var a = el('a', { href: src, target: '_blank', rel: 'noopener noreferrer' });
    a.textContent = 'Open the video file directly';
    box.appendChild(p);
    box.appendChild(a);
    player.appendChild(box);
  }

  function play(i, scroll, auto) {
    var ep = episodes[i];
    if (!ep) return;
    current = i;

    player.textContent = '';
    var src = ep.getAttribute('data-src');
    if (ep.getAttribute('data-player') === 'video') {
      var mime = ep.getAttribute('data-mime') || 'video/mp4';
      var video = el('video', { controls: '', playsinline: '', preload: 'metadata' });
      if (!video.canPlayType(mime)) {
        failed(src, 'This browser can’t play this stream (' + mime + '). Try Safari, or open it in a player like VLC.');
      } else {
        video.autoplay = auto !== false;
        var source = el('source', { src: src, type: mime });
        source.addEventListener('error', function () { failed(src, 'This video could not be loaded. The source may be offline.'); });
        video.appendChild(source);
        video.addEventListener('ended', function () { play(current + 1, false); });
        player.appendChild(video);
      }
    } else {
      player.appendChild(el('iframe', {
        src: src,
        allow: 'autoplay; fullscreen; picture-in-picture; encrypted-media',
        allowfullscreen: '',
        referrerpolicy: 'no-referrer'
      }));
    }

    episodes.forEach(function (e) { e.classList.toggle('active', e === ep); });
    nowPlaying.textContent = ep.getAttribute('data-label');
    prevBtn.disabled = i <= 0;
    nextBtn.disabled = i >= episodes.length - 1;
    history.replaceState(null, '', '#' + ep.id);
    try { localStorage.setItem(storeKey, ep.id); } catch (e) {}
    if (scroll) wrap.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }

  episodes.forEach(function (ep, i) {
    ep.addEventListener('click', function () { play(i, true); });
  });
  prevBtn.addEventListener('click', function () { play(current - 1, false); });
  nextBtn.addEventListener('click', function () { play(current + 1, false); });

  // Resume from the URL hash, else the last episode watched on this device.
  var start = location.hash.slice(1);
  if (!start) {
    try { start = localStorage.getItem(storeKey) || ''; } catch (e) {}
  }
  var idx = episodes.findIndex(function (e) { return e.id === start; });
  if (idx >= 0) {
    play(idx, !!location.hash, false);
  } else if (episodes.length) {
    play(0, false, false);
  }
})();

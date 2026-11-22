'use strict';

// 汽水音乐搜索代理。
// 背景：汽水官方搜索接口 api.qishui.com/luna/pc/search/track 需要客户端原生签名
//       (x-helios / x-medusa)，未签名请求返回 HTTP 200 + 空 body，导致 go-music-api
//       的 soda.Search 拿到空结果。这里把搜索请求改走免签名的公开搜索接口，其余路径
//       （取链 / 榜单 / cookies / 扫码登录等）原样转发给 go-music-api。
//
// 用法：见同目录 README.md。容器内需要 Node 运行时（复用 meting-api 镜像即可）。

const http = require('http');
const { URL } = require('url');

const PORT = 8080;
const UPSTREAM = process.env.QISHUI_UPSTREAM || 'http://gomusic:8080';
const PUBLIC_SEARCH = 'https://api-vehicle.volcengine.com/v2/search/type';

function sendJSON(res, status, obj) {
  const body = Buffer.from(JSON.stringify(obj));
  res.writeHead(status, {
    'Content-Type': 'application/json; charset=utf-8',
    'Content-Length': body.length,
    'Access-Control-Allow-Origin': '*',
  });
  res.end(body);
}

function firstText(...values) {
  for (const value of values) {
    if (value === null || value === undefined) continue;
    const text = String(value).trim();
    if (text) return text;
  }
  return '';
}

function artistFrom(item) {
  const info = item.author_info || item.author || {};
  if (typeof info === 'string') return info;
  return firstText(
    info.name,
    info.nickname,
    Array.isArray(item.authors)
      ? item.authors.map((a) => (a && a.name) || a).filter(Boolean).join(' / ')
      : '',
    item.author_name,
    item.artist_name,
  ) || '未知歌手';
}

async function handleSearch(req, res, parsed) {
  const keyword = firstText(parsed.searchParams.get('q'), parsed.searchParams.get('keyword'));
  let limit = parseInt(parsed.searchParams.get('limit') || '10', 10);
  if (!Number.isFinite(limit) || limit <= 0 || limit > 30) limit = 10;

  if (!keyword) {
    return sendJSON(res, 200, {
      code: 200,
      msg: 'success',
      data: { albums: null, playlists: null, songs: [], type: 'song' },
    });
  }

  const target = new URL(PUBLIC_SEARCH);
  target.searchParams.set('keyword', keyword);
  target.searchParams.set('search_type', 'music');
  target.searchParams.set('limit', String(Math.max(30, limit)));
  target.searchParams.set('real_offset', '0');
  target.searchParams.set('search_source', 'qishui');

  try {
    const response = await fetch(target, {
      headers: { Accept: 'application/json' },
      signal: AbortSignal.timeout(15000),
    });
    const json = await response.json();
    const list = Array.isArray(json && json.data && json.data.list) ? json.data.list : [];
    const songs = [];
    const seen = new Set();

    for (const item of list) {
      if (item && item.media_type && item.media_type !== 'music') continue;
      const id = firstText(item && item.item_id, item && item.id);
      const name = firstText(item && item.title, item && item.name);
      if (!id || !name || seen.has(id)) continue;
      seen.add(id);

      songs.push({
        id,
        name,
        artist: artistFrom(item || {}),
        album: firstText(
          item.collection_name,
          item.album_name,
          item.album && item.album.name,
        ),
        duration: Number(item.duration || 0),
        cover: firstText(item.cover_url),
        source: 'soda',
        url: '',
        ext: '',
        link: '',
        extra: {},
      });
      if (songs.length >= limit) break;
    }

    return sendJSON(res, 200, {
      code: 200,
      msg: 'success',
      data: { albums: null, playlists: null, songs, type: 'song' },
    });
  } catch (error) {
    console.error('[qishui-proxy] public search failed:', error && error.message);
    return sendJSON(res, 502, {
      code: 502,
      msg: 'public search failed',
      data: { albums: null, playlists: null, songs: [], type: 'song' },
    });
  }
}

function proxyToUpstream(req, res, parsed) {
  const target = new URL(UPSTREAM);
  const headers = Object.assign({}, req.headers);
  delete headers.host;

  const options = {
    hostname: target.hostname,
    port: target.port || 80,
    path: parsed.pathname + parsed.search,
    method: req.method,
    headers,
  };

  const upstream = http.request(options, (upstreamRes) => {
    res.writeHead(upstreamRes.statusCode || 502, upstreamRes.headers);
    upstreamRes.pipe(res);
  });

  upstream.on('error', (error) => {
    console.error('[qishui-proxy] upstream error:', error && error.message);
    if (!res.headersSent) {
      sendJSON(res, 502, { code: 502, msg: 'upstream unavailable', data: null });
    } else {
      res.end();
    }
  });

  req.pipe(upstream);
}

const server = http.createServer((req, res) => {
  const parsed = new URL(req.url || '/', 'http://localhost');
  if (parsed.pathname === '/api/v1/music/search') {
    return handleSearch(req, res, parsed);
  }
  return proxyToUpstream(req, res, parsed);
});

server.listen(PORT, '0.0.0.0', () => {
  console.log('[qishui-proxy] listening on 0.0.0.0:' + PORT + ' -> ' + UPSTREAM);
});

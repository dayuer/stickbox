(function () {
  'use strict';

  var token = new URLSearchParams(location.search).get('t') || '';
  var $ = function (id) { return document.getElementById(id); };
  var done = false; // 配对成功后不再提供配对入口

  // 一律用 textContent 写入页面，不拼 HTML。
  function setMsg(el, text, kind) {
    el.textContent = text || '';
    el.className = 'msg' + (kind ? ' ' + kind : '');
  }

  var NET_LOST = '和本机的 stickbox-agent 断开了（它可能已自动退出）。请回到终端重新运行 stickbox-agent ui。';

  // 返回 {status, body}；网络层失败时 body.e === 'local_down'。
  function api(method, path, payload) {
    var opts = { method: method, headers: { 'X-Stickbox-Token': token }, cache: 'no-store' };
    if (method === 'POST') {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(payload || {});
    }
    return fetch(path, opts).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (b) {
        return { status: r.status, body: b };
      });
    }, function () {
      return { status: 0, body: { e: 'local_down', msg: NET_LOST } };
    });
  }

  function fmtTime(ts) {
    return ts ? new Date(ts * 1000).toLocaleString('zh-CN', { hour12: false }) : '';
  }

  function failText(res) {
    var b = res.body || {};
    var t = b.msg || ('出错了（HTTP ' + res.status + '）');
    if (b.retry_after) { t += '（约 ' + b.retry_after + ' 秒后可再试）'; }
    return t;
  }

  // ---------- 6 位输入框 ----------
  var boxes = Array.prototype.slice.call(document.querySelectorAll('#code-boxes input'));
  var btnPair = $('btn-pair');

  function code() { return boxes.map(function (b) { return b.value; }).join(''); }
  function refreshPairBtn() { btnPair.disabled = done || code().length !== 6; }

  function fillFrom(start, digits) {
    var i = start;
    for (var k = 0; k < digits.length && i < boxes.length; k++, i++) { boxes[i].value = digits[k]; }
    var next = Math.min(i, boxes.length - 1);
    boxes[next].focus();
    refreshPairBtn();
  }

  boxes.forEach(function (box, idx) {
    box.addEventListener('input', function () {
      var digits = box.value.replace(/\D/g, '');
      box.value = '';
      if (digits) { fillFrom(idx, digits); } else { refreshPairBtn(); }
    });
    box.addEventListener('keydown', function (e) {
      if (e.key === 'Backspace' && !box.value && idx > 0) {
        boxes[idx - 1].value = '';
        boxes[idx - 1].focus();
        refreshPairBtn();
        e.preventDefault();
      } else if (e.key === 'ArrowLeft' && idx > 0) {
        boxes[idx - 1].focus();
      } else if (e.key === 'ArrowRight' && idx < boxes.length - 1) {
        boxes[idx + 1].focus();
      }
    });
    box.addEventListener('focus', function () { box.select(); });
    box.addEventListener('paste', function (e) {
      var text = (e.clipboardData || window.clipboardData).getData('text') || '';
      var digits = text.replace(/\D/g, '').slice(0, 6);
      e.preventDefault();
      if (digits) {
        boxes.forEach(function (b) { b.value = ''; });
        fillFrom(0, digits);
      }
    });
  });

  function clearBoxes() {
    boxes.forEach(function (b) { b.value = ''; });
    boxes[0].focus();
    refreshPairBtn();
  }

  function onPaired(msg) {
    done = true;
    setMsg($('pair-msg'), msg || '已配对，电视会在几秒内切到看板', 'ok');
    refreshPairBtn();
    boxes.forEach(function (b) { b.disabled = true; });
    Array.prototype.forEach.call(document.querySelectorAll('#found-list button'), function (b) { b.disabled = true; });
    loadDevices();
    loadStatus();
  }

  $('pair-form').addEventListener('submit', function (e) {
    e.preventDefault();
    if (done || code().length !== 6) { return; }
    btnPair.disabled = true;
    setMsg($('pair-msg'), '正在配对……');
    api('POST', '/api/pair', { code: code() }).then(function (res) {
      if (res.status === 200 && res.body.ok) {
        onPaired(res.body.msg);
      } else {
        setMsg($('pair-msg'), failText(res), 'err');
        if (res.body && res.body.e === 'bad_code') { clearBoxes(); }
        refreshPairBtn();
      }
    });
  });

  // ---------- 发现 ----------
  function renderFound(list) {
    var ul = $('found-list');
    ul.textContent = '';
    list.forEach(function (d) {
      var li = document.createElement('li');
      var info = document.createElement('div');
      var title = document.createElement('div');
      title.textContent = '屏幕 …' + d.short;
      var meta = document.createElement('div');
      meta.className = 'meta';
      meta.textContent = '固件 ' + (d.fw || '未知') + ' · ' + d.ip;
      info.appendChild(title);
      info.appendChild(meta);
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.textContent = '配对';
      btn.disabled = done;
      btn.addEventListener('click', function () { pairFound(d, btn); });
      li.appendChild(info);
      li.appendChild(btn);
      ul.appendChild(li);
    });
  }

  function discover() {
    var hint = $('discover-hint');
    $('btn-discover').disabled = true;
    hint.textContent = '正在局域网里搜索……（约 2 秒）';
    api('GET', '/api/discover').then(function (res) {
      $('btn-discover').disabled = false;
      if (res.status !== 200) {
        hint.textContent = failText(res) + ' 你仍然可以在下面手动输入配对码。';
        renderFound([]);
        return;
      }
      var list = res.body.devices || [];
      renderFound(list);
      hint.textContent = list.length
        ? '找到 ' + list.length + ' 块待配对的屏幕，点「配对」即可，不用输码。'
        : '没发现待配对的屏幕。请确认电视上正显示配对码、电脑与电视在同一个网络；或在下面手动输入。';
    });
  }

  function pairFound(d, btn) {
    btn.disabled = true;
    setMsg($('pair-msg'), '正在向 …' + d.short + ' 配对……');
    api('POST', '/api/pair-device', { dev: d.dev }).then(function (res) {
      if (res.status === 200 && res.body.ok) {
        onPaired(res.body.msg);
      } else {
        btn.disabled = done;
        setMsg($('pair-msg'), failText(res), 'err');
        if (res.body && (res.body.e === 'not_pending' || res.body.e === 'code_expired' || res.body.e === 'not_found')) { discover(); }
      }
    });
  }

  // ---------- 设备 ----------
  function loadDevices() {
    var hint = $('devices-hint');
    api('GET', '/api/devices').then(function (res) {
      var ul = $('device-list');
      ul.textContent = '';
      if (res.status === 409 && res.body.e === 'not_paired') {
        hint.textContent = '还没有配对。配好之后设备会显示在这里。';
        return;
      }
      if (res.status !== 200) {
        hint.textContent = failText(res);
        return;
      }
      var list = res.body.devices || [];
      hint.textContent = list.length ? '' : '名下还没有屏幕。';
      list.forEach(function (d) {
        var li = document.createElement('li');
        var info = document.createElement('div');
        var title = document.createElement('div');
        title.textContent = (d.name || '（未命名）') + (d.online ? ' · 在线' : ' · 离线');
        var meta = document.createElement('div');
        meta.className = 'meta';
        meta.textContent = d.dev + (d.bound ? ' · 绑定于 ' + fmtTime(d.bound) : '');
        info.appendChild(title);
        info.appendChild(meta);
        var btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'danger';
        btn.textContent = '解绑';
        btn.addEventListener('click', function () {
          if (!window.confirm('确定解绑 ' + (d.name || d.dev) + '？解绑后这块屏会重新显示配对码。')) { return; }
          btn.disabled = true;
          api('POST', '/api/unpair', { dev: d.dev }).then(function (r) {
            setMsg($('devices-msg'), r.status === 200 ? (r.body.msg || '已解绑') : failText(r), r.status === 200 ? 'ok' : 'err');
            loadDevices();
          });
        });
        li.appendChild(info);
        li.appendChild(btn);
        ul.appendChild(li);
      });
    });
  }

  // ---------- 状态 ----------
  function addRow(dl, k, v) {
    var dt = document.createElement('dt');
    dt.textContent = k;
    var dd = document.createElement('dd');
    dd.textContent = v;
    dl.appendChild(dt);
    dl.appendChild(dd);
  }

  function loadStatus() {
    api('GET', '/api/status').then(function (res) {
      var dl = $('status-list');
      dl.textContent = '';
      if (res.status !== 200) {
        setMsg($('status-msg'), failText(res), 'err');
        return;
      }
      var s = res.body;
      addRow(dl, '版本', s.version);
      addRow(dl, '服务器', s.server);
      addRow(dl, '配对', s.paired ? '已配对' + (s.paired_at ? '（' + fmtTime(s.paired_at) + '）' : '') : '未配对');
      var auth = { logged_in: '已登录', logged_out: '未登录，请在终端执行 claude auth login' }[s.claude_auth] || '未知（还没确认过）';
      addRow(dl, 'claude 登录', auth);
      if (!s.claude_found) { addRow(dl, 'claude 程序', 'PATH 里没找到'); }
      addRow(dl, '上次成功', s.last_success_at ? fmtTime(s.last_success_at) : '无记录');
      addRow(dl, '最近错误', s.last_error ? s.last_error + '（' + fmtTime(s.last_error_at) + '）' : '无');
      if (s.consecutive_failures > 0) {
        addRow(dl, '连续失败', s.consecutive_failures + ' 轮' + (s.next_retry_at ? '，下次尝试 ' + fmtTime(s.next_retry_at) : ''));
      }
      if (s.cooldown) { addRow(dl, '冷却中', '用量接口限流，agent 在逐步拉长重试间隔，无需处理'); }
      addRow(dl, '后台服务', s.service_installed ? '已安装' : '未安装（终端运行 stickbox-agent install）');
      if (s.insecure) { addRow(dl, 'TLS', '已关闭证书校验（--insecure）'); }
    });
  }

  $('btn-run').addEventListener('click', function () {
    var btn = $('btn-run');
    btn.disabled = true;
    setMsg($('status-msg'), '正在上报，可能要几十秒……');
    api('POST', '/api/run-once', {}).then(function (res) {
      btn.disabled = false;
      setMsg($('status-msg'), res.status === 200 ? (res.body.msg || '完成') : failText(res), res.status === 200 ? 'ok' : 'err');
      loadStatus();
    });
  });

  $('btn-discover').addEventListener('click', discover);
  $('btn-devices').addEventListener('click', loadDevices);

  boxes[0].focus();
  loadStatus();
  loadDevices();
  discover();
})();

<script setup lang="ts">
// 「我的文件」页：把群晖共享文件夹（数据根顶层的 btrfs 子卷）迁入飞牛个人空间
// 目录 /volN/<uid>/，使其在飞牛「我的文件」中显示为文件夹。
// mv = 同文件系统 rename，瞬间完成、零拷贝；挪回去即可还原（可逆性极强）。
import { onActivated, onMounted, ref } from 'vue'
import { api, type AdoptedVolume, type ShareMoveResult, type ShareView } from '../api'
import UiBadge from './ui/UiBadge.vue'
import UiButton from './ui/UiButton.vue'
import UiCard from './ui/UiCard.vue'
import UiEmpty from './ui/UiEmpty.vue'

interface Row {
  vol: AdoptedVolume
  view: ShareView | null
  err: string
  sel: string[]          // 勾选的待迁移目录名
  busy: boolean
}

const rows = ref<Row[]>([])
// 迁移结果按卷（vgUuid）单独存放——不能挂在 Row 上：move 成功后要 load() 重扫，
// 而 load 会重建整个 rows 数组，挂在 Row 上的结果会随旧对象一起被丢弃
// （真机实测 2026-10-30：部分失败的迁移结果"一闪就消失"）。
const results = ref<Record<string, ShareMoveResult[] | null>>({})
const loading = ref(false)
const error = ref('')

async function load() {
  loading.value = true
  error.value = ''
  try {
    const res = await api.adopted()
    const vols = (res.volumes ?? []).filter((v) => v.mounted && v.mountPoint)
    const next: Row[] = []
    for (const vol of vols) {
      const row: Row = { vol, view: null, err: '', sel: [], busy: false }
      try {
        const sv = await api.sharesScan(vol.mountPoint)
        row.view = sv.data
        row.sel = (sv.data.items ?? []).map((i) => i.name) // 默认全选非系统目录
      } catch (e) {
        row.err = e instanceof Error ? e.message : String(e)
      }
      next.push(row)
    }
    rows.value = next
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

function toggle(row: Row, name: string) {
  row.sel = row.sel.includes(name) ? row.sel.filter((n) => n !== name) : [...row.sel, name]
}

async function move(row: Row) {
  if (!row.view || row.sel.length === 0) return
  const mp = row.vol.mountPoint
  const list = row.sel.map((n) => `${mp}/${n} → ${mp}/${row.view!.uid}/${n}`).join('\n')
  if (!window.confirm(`确认把以下目录迁入「我的文件」（${mp}/${row.view.uid}/）？\n\n${list}\n\n瞬间完成、零拷贝；挪回去即可还原。`)) return
  row.busy = true
  results.value[row.vol.vgUuid] = null // 清掉上一轮结果
  try {
    const res = await api.sharesMove(mp, row.view.uid, row.sel)
    results.value[row.vol.vgUuid] = res.results ?? []
    await load() // 重扫刷新两侧列表；results 独立存放不受重建影响
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e)
    error.value = msg
    // HTTP 失败同样落卡内常驻显示，顶部 error 会在下次 load 时被清
    results.value[row.vol.vgUuid] = [{ name: '迁移请求', ok: false, err: msg }]
  } finally {
    row.busy = false
  }
}

onMounted(load)
onActivated(load)
</script>

<template>
  <UiCard title="共享文件夹 → 我的文件">
    <template #desc>
      群晖共享文件夹物理上就在已接管卷的数据根顶层，只是飞牛「我的文件」只扫描个人空间目录
      <span class="mono">/volN/&lt;uid&gt;/</span>。这里把它们迁入个人空间目录即可显示——
      mv 在同一文件系统内是 rename：<b>瞬间完成、零拷贝、与数据量无关</b>；
      不想要时挪回去即可还原。注意：回群晖时路径会多一层（<span class="mono">@syno/&lt;uid&gt;/&lt;name&gt;</span>），DSM 共享文件夹需重新指向。
    </template>
    <template #actions>
      <UiButton :disabled="loading" @click="load">{{ loading ? '扫描中…' : '刷新' }}</UiButton>
    </template>

    <p v-if="error" class="err-text">✗ {{ error }}</p>

    <UiEmpty v-if="!loading && rows.length === 0">
      没有已挂载的接管卷
      <template #hint>先到「磁盘接管」完成接管并挂载，这里才会出现可迁移的共享文件夹</template>
    </UiEmpty>

    <div v-for="row in rows" :key="row.vol.vgUuid" class="vol-card">
      <div class="vol-head">
        <span class="vol-title">
          存储空间 {{ row.vol.volId }} <span class="mono">· {{ row.vol.mountPoint }}</span>
        </span>
        <span class="vol-disk">{{ row.vol.disk }}</span>
      </div>

      <p v-if="row.err" class="err-text">✗ 扫描失败：{{ row.err }}</p>

      <template v-else-if="row.view">
        <p class="hint">
          个人空间：<span class="mono">{{ row.vol.mountPoint }}/{{ row.view.uid || '?' }}</span>
          <template v-if="row.view.uidExists">（已存在）</template>
          <template v-else>（不存在，迁移时自动创建）</template>
        </p>

        <div v-if="(row.view.items ?? []).length" class="item-list">
          <label v-for="it in row.view.items" :key="it.name" class="item">
            <input type="checkbox" :checked="row.sel.includes(it.name)" @change="toggle(row, it.name)" />
            <span class="item-name">{{ it.name }}</span>
            <UiBadge v-if="it.isSubvol" type="info">子卷</UiBadge>
          </label>
        </div>
        <p v-else class="hint">数据根顶层没有可迁移的用户目录。</p>

        <p v-if="(row.view.system ?? []).length" class="hint sys-line-note">
          系统目录（不可迁移）：{{ (row.view.system ?? []).map((s) => s.name).join('、') }}
        </p>

        <p v-if="(row.view.migrated ?? []).length" class="hint">
          已在「我的文件」中：{{ (row.view.migrated ?? []).join('、') }}
        </p>

        <div class="act-line">
          <UiButton :disabled="row.busy || row.sel.length === 0 || !row.view.uid" @click="move(row)">
            {{ row.busy ? '迁移中…' : `迁入选中的 ${row.sel.length} 个目录` }}
          </UiButton>
        </div>

        <div v-if="results[row.vol.vgUuid]" class="result">
          <div
            v-for="r in results[row.vol.vgUuid] ?? []" :key="r.name"
            class="res-line" :class="r.ok ? 'res-ok' : 'res-fail'"
          >{{ r.ok ? '✓' : '✗' }} {{ r.name }}<template v-if="r.err"> — {{ r.err }}</template><template v-else-if="r.note"> — {{ r.note }}</template></div>
        </div>
      </template>
    </div>
  </UiCard>
</template>

<style scoped>
.vol-card {
  border: 1px solid var(--fui-border);
  border-radius: 8px;
  padding: 12px 14px;
  margin-bottom: 10px;
}
.vol-head { display: flex; align-items: baseline; gap: 10px; flex-wrap: wrap; }
.vol-title { font-size: 13px; font-weight: 600; }
.vol-disk { font-size: 12px; color: var(--fui-text-2); }

.item-list { margin: 8px 0; }
.item {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  padding: 5px 10px;
  margin: 0 8px 8px 0;
  border: 1px solid var(--fui-border);
  border-radius: 6px;
  cursor: pointer;
  background: var(--fui-panel);
}
.item input { accent-color: var(--fui-primary); }
.item-name { font-size: 13px; }

.sys-line-note { margin-top: 4px; }
.act-line { margin-top: 10px; }

.result {
  margin-top: 10px;
  border-radius: 6px;
  padding: 8px 12px;
  background: var(--fui-panel);
}
.res-line { font-size: 12px; word-break: break-all; }
.res-ok { color: var(--fui-green); }
.res-fail { color: var(--fui-red); }
.err-text { color: var(--fui-red); font-size: 13px; }
</style>

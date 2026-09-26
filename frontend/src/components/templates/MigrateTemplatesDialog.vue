<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Label } from '@/components/ui/label'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { templatesService, type TemplateMigrationPreview, type TemplateMigrationResult } from '@/services/api'
import { defaultSelectedIds, describeFailures, toggleId, type MigrationFailure } from '@/lib/templateMigration'
import { getErrorMessage } from '@/lib/api-utils'
import { getQualityBadgeClass, getQualityRatingLabel } from '@/lib/utils'
import { toast } from 'vue-sonner'
import { AlertTriangle, ArrowRightLeft, CheckCircle2, Loader2, XCircle } from 'lucide-vue-next'

interface AccountOption {
  id: string
  name: string
  business_id?: string
}

interface Props {
  open: boolean
  /** Destination account name (the currently selected account on the Templates page). */
  destinationAccount: string
  /** All connected accounts; the destination is filtered out of the source picker. */
  accounts: AccountOption[]
}

const props = defineProps<Props>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  /** Fired after a migration ran (successfully or partially) so the parent can refresh. */
  migrated: [result: TemplateMigrationResult]
}>()

const { t } = useI18n()

type Step = 'source' | 'preview' | 'result'
type SourceMode = 'account' | 'waba'

const step = ref<Step>('source')
const sourceMode = ref<SourceMode>('account')
const sourceAccount = ref<string>('')
const sourceWabaId = ref<string>('')
const isLoadingPreview = ref(false)
const isMigrating = ref(false)
const preview = ref<TemplateMigrationPreview | null>(null)
const selectedIds = ref<string[]>([])
const result = ref<TemplateMigrationResult | null>(null)
const failures = ref<MigrationFailure[]>([])
const resultError = ref<string | null>(null)

const sourceAccounts = computed(() => props.accounts.filter(a => a.name !== props.destinationAccount))

const sourcePayload = computed(() => {
  if (sourceMode.value === 'account') {
    return sourceAccount.value ? { source_whatsapp_account: sourceAccount.value } : null
  }
  const waba = sourceWabaId.value.trim()
  return waba ? { source_waba_id: waba } : null
})

const canPreview = computed(() => !!sourcePayload.value && !isLoadingPreview.value)

// "Select all" follows the default selection: eligible and not already on the
// destination. Duplicates stay an explicit per-row choice.
const selectableIds = computed(() => defaultSelectedIds(preview.value?.templates ?? []))
const allSelectableSelected = computed(
  () => selectableIds.value.length > 0 && selectableIds.value.every(id => selectedIds.value.includes(id))
)

function reset() {
  step.value = 'source'
  sourceMode.value = sourceAccounts.value.length > 0 ? 'account' : 'waba'
  sourceAccount.value = ''
  sourceWabaId.value = ''
  preview.value = null
  selectedIds.value = []
  result.value = null
  failures.value = []
  resultError.value = null
  isLoadingPreview.value = false
  isMigrating.value = false
}

watch(
  () => props.open,
  isOpen => {
    if (isOpen) reset()
  }
)

function close() {
  emit('update:open', false)
}

async function loadPreview() {
  if (!sourcePayload.value) return
  isLoadingPreview.value = true
  try {
    const response = await templatesService.migratePreview({
      whatsapp_account: props.destinationAccount,
      ...sourcePayload.value
    })
    preview.value = response.data.data
    selectedIds.value = defaultSelectedIds(preview.value.templates)
    step.value = 'preview'
  } catch (error) {
    toast.error(getErrorMessage(error, t('templates.migrate.previewFailed')))
  } finally {
    isLoadingPreview.value = false
  }
}

function isSelected(id: string): boolean {
  return selectedIds.value.includes(id)
}

function setSelected(id: string, checked: boolean | 'indeterminate') {
  selectedIds.value = toggleId(selectedIds.value, id, checked === true)
}

function toggleAllSelectable() {
  selectedIds.value = allSelectableSelected.value ? [] : [...selectableIds.value]
}

async function runMigration() {
  if (!preview.value || selectedIds.value.length === 0 || !sourcePayload.value) return
  isMigrating.value = true
  resultError.value = null
  try {
    const response = await templatesService.migrate({
      whatsapp_account: props.destinationAccount,
      ...sourcePayload.value,
      template_ids: selectedIds.value
    })
    result.value = response.data.data
  } catch (error: any) {
    // A 502 part-way still carries the partial result in the error envelope;
    // any other failure (validation, network) leaves nothing to show, so stay
    // on the preview with the selection intact.
    const partial = error?.response?.data?.data as TemplateMigrationResult | undefined
    const message = getErrorMessage(error, t('templates.migrate.migrationFailed'))
    if (!partial || typeof partial.migrated_count !== 'number') {
      toast.error(message)
      isMigrating.value = false
      return
    }
    resultError.value = message
    result.value = partial
  } finally {
    isMigrating.value = false
  }

  if (result.value) {
    failures.value = describeFailures(result.value, preview.value.templates)
    if (result.value.migrated_count > 0 || result.value.synced_count > 0) {
      emit('migrated', result.value)
    }
    if (!resultError.value && result.value.migrated_count > 0) {
      toast.success(t('templates.migrate.migratedToast', { count: result.value.migrated_count }))
    }
  }
  step.value = 'result'
}

</script>

<template>
  <Dialog :open="open" @update:open="emit('update:open', $event)">
    <DialogContent class="sm:max-w-2xl">
      <DialogHeader>
        <DialogTitle class="flex items-center gap-2">
          <ArrowRightLeft class="h-5 w-5" />
          {{ $t('templates.migrate.title') }}
        </DialogTitle>
        <DialogDescription>
          {{ $t('templates.migrate.description', { account: destinationAccount }) }}
        </DialogDescription>
      </DialogHeader>

      <!-- Step 1: choose the source WABA -->
      <div v-if="step === 'source'" class="space-y-4">
        <Alert>
          <AlertTriangle class="h-4 w-4" />
          <AlertTitle>{{ $t('templates.migrate.preconditionsTitle') }}</AlertTitle>
          <AlertDescription>
            <ul class="list-disc pl-4 space-y-1 text-sm">
              <li>{{ $t('templates.migrate.preconditionBusiness') }}</li>
              <li>{{ $t('templates.migrate.preconditionEligible') }}</li>
              <li>{{ $t('templates.migrate.preconditionQuality') }}</li>
              <li>{{ $t('templates.migrate.preconditionBspOwned') }}</li>
            </ul>
          </AlertDescription>
        </Alert>

        <div class="space-y-2">
          <Label>{{ $t('templates.migrate.sourceLabel') }}</Label>
          <RadioGroup v-model="sourceMode" class="flex flex-col gap-2">
            <div class="flex items-center space-x-2">
              <RadioGroupItem id="migrate-source-account" value="account" :disabled="sourceAccounts.length === 0" />
              <Label for="migrate-source-account" class="cursor-pointer font-normal">
                {{ $t('templates.migrate.sourceConnected') }}
              </Label>
            </div>
            <div class="flex items-center space-x-2">
              <RadioGroupItem id="migrate-source-waba" value="waba" />
              <Label for="migrate-source-waba" class="cursor-pointer font-normal">
                {{ $t('templates.migrate.sourceWabaId') }}
              </Label>
            </div>
          </RadioGroup>
        </div>

        <div v-if="sourceMode === 'account'" class="space-y-2">
          <Label for="migrate-source-account-select">{{ $t('templates.migrate.sourceAccount') }}</Label>
          <Select v-model="sourceAccount">
            <SelectTrigger id="migrate-source-account-select">
              <SelectValue :placeholder="$t('templates.selectAccount')" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem v-for="account in sourceAccounts" :key="account.id" :value="account.name">
                {{ account.name }}
              </SelectItem>
            </SelectContent>
          </Select>
          <p class="text-xs text-muted-foreground">{{ $t('templates.migrate.sourceConnectedHint') }}</p>
        </div>

        <div v-else class="space-y-2">
          <Label for="migrate-source-waba-input">{{ $t('templates.migrate.sourceWabaId') }}</Label>
          <Input id="migrate-source-waba-input" v-model="sourceWabaId" placeholder="1234567890123456" class="font-mono" />
          <p class="text-xs text-muted-foreground">{{ $t('templates.migrate.sourceWabaHint') }}</p>
        </div>

        <div class="flex justify-end gap-2 pt-2">
          <Button variant="outline" @click="close">{{ $t('common.cancel') }}</Button>
          <Button :disabled="!canPreview" @click="loadPreview">
            <Loader2 v-if="isLoadingPreview" class="h-4 w-4 mr-2 animate-spin" />
            {{ $t('templates.migrate.previewButton') }}
          </Button>
        </div>
      </div>

      <!-- Step 2: pick templates -->
      <div v-else-if="step === 'preview' && preview" class="space-y-4">
        <div class="flex items-center justify-between text-sm">
          <div class="text-muted-foreground">
            {{ $t('templates.migrate.previewSummary', { total: preview.total, eligible: preview.eligible, source: preview.source_label }) }}
          </div>
          <Button variant="ghost" size="sm" :disabled="selectableIds.length === 0" @click="toggleAllSelectable">
            {{ allSelectableSelected ? $t('common.deselectAll') : $t('common.selectAll') }}
          </Button>
        </div>

        <div v-if="preview.templates.length === 0" class="py-8 text-center text-sm text-muted-foreground">
          {{ $t('templates.migrate.noTemplates') }}
        </div>

        <ScrollArea v-else class="h-80 border rounded-md">
          <div class="divide-y">
            <div
              v-for="item in preview.templates"
              :key="item.id"
              class="flex items-start gap-3 p-3"
              :class="{ 'opacity-60': !item.eligible }"
            >
              <Checkbox
                :id="'migrate-' + item.id"
                :checked="isSelected(item.id)"
                :disabled="!item.eligible"
                class="mt-1"
                @update:checked="setSelected(item.id, $event)"
              />
              <div class="flex-1 space-y-1">
                <Label :for="'migrate-' + item.id" class="flex items-center gap-2 flex-wrap cursor-pointer font-normal">
                  <span class="font-mono text-sm">{{ item.name }}</span>
                  <Badge variant="outline" class="text-xs">{{ item.language }}</Badge>
                  <Badge variant="outline" class="text-xs">{{ item.category }}</Badge>
                  <Badge :class="getQualityBadgeClass(item.quality_rating || 'UNKNOWN')" class="text-xs">{{ getQualityRatingLabel(item.quality_rating || 'UNKNOWN', t) }}</Badge>
                  <Badge v-if="item.exists_on_destination" variant="warning" class="text-xs">
                    {{ $t('templates.migrate.existsOnDestination') }}
                  </Badge>
                </Label>
                <p v-if="!item.eligible" class="text-xs text-muted-foreground">{{ item.reason }}</p>
                <p v-else-if="item.exists_on_destination" class="text-xs text-muted-foreground">
                  {{ $t('templates.migrate.existsOnDestinationHint') }}
                </p>
              </div>
            </div>
          </div>
        </ScrollArea>

        <div class="flex justify-between gap-2 pt-2">
          <Button variant="outline" :disabled="isMigrating" @click="step = 'source'">{{ $t('common.back') }}</Button>
          <div class="flex gap-2">
            <Button variant="outline" :disabled="isMigrating" @click="close">{{ $t('common.cancel') }}</Button>
            <Button :disabled="selectedIds.length === 0 || isMigrating" @click="runMigration">
              <Loader2 v-if="isMigrating" class="h-4 w-4 mr-2 animate-spin" />
              <ArrowRightLeft v-else class="h-4 w-4 mr-2" />
              {{ $t('templates.migrate.migrateButton', { count: selectedIds.length }) }}
            </Button>
          </div>
        </div>
      </div>

      <!-- Step 3: result -->
      <div v-else-if="step === 'result'" class="space-y-4">
        <Alert v-if="resultError" variant="destructive">
          <XCircle class="h-4 w-4" />
          <AlertTitle>{{ $t('templates.migrate.migrationFailed') }}</AlertTitle>
          <AlertDescription>{{ resultError }}</AlertDescription>
        </Alert>

        <template v-if="result">
          <div class="grid grid-cols-3 gap-3 text-center">
            <div class="rounded-md border p-3">
              <div class="text-2xl font-semibold text-green-500">{{ result.migrated_count }}</div>
              <div class="text-xs text-muted-foreground">{{ $t('templates.migrate.migratedCount') }}</div>
            </div>
            <div class="rounded-md border p-3">
              <div class="text-2xl font-semibold" :class="result.failed_count > 0 ? 'text-red-500' : ''">{{ result.failed_count }}</div>
              <div class="text-xs text-muted-foreground">{{ $t('templates.migrate.failedCount') }}</div>
            </div>
            <div class="rounded-md border p-3">
              <div class="text-2xl font-semibold">{{ result.synced_count }}</div>
              <div class="text-xs text-muted-foreground">{{ $t('templates.migrate.syncedCount') }}</div>
            </div>
          </div>

          <Alert v-if="result.sync_error" variant="warning">
            <AlertTriangle class="h-4 w-4" />
            <AlertDescription>{{ result.sync_error }}</AlertDescription>
          </Alert>

          <p class="text-xs text-muted-foreground flex items-start gap-2">
            <CheckCircle2 class="h-4 w-4 mt-0.5 shrink-0" />
            {{ $t('templates.migrate.qualityResetNote') }}
          </p>

          <div v-if="failures.length > 0" class="space-y-2">
            <Label>{{ $t('templates.migrate.failuresTitle') }}</Label>
            <div class="max-h-48 overflow-y-auto border rounded-md divide-y">
              <div v-for="failure in failures" :key="failure.id" class="p-3 text-sm">
                <span class="font-mono">{{ failure.name }}</span>
                <span class="text-muted-foreground"> — {{ failure.reason }}</span>
              </div>
            </div>
          </div>
        </template>

        <div class="flex justify-end gap-2 pt-2">
          <Button @click="close">{{ $t('common.close') }}</Button>
        </div>
      </div>
    </DialogContent>
  </Dialog>
</template>

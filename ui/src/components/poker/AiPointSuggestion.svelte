<script lang="ts">
  import { Lightbulb, Loader2 } from 'lucide-svelte';
  import LL from '../../i18n/i18n-svelte';

  export let description = '';
  export let acceptanceCriteria = '';
  export let storyName = '';
  export let points = ['1', '2', '3', '5', '8', '13', '?'];

  let isLoading = false;
  let aiSuggestion = null;
  let errorMessage = '';

  // 请求AI建议的函数
  async function requestAiSuggestion() {
    if (!description && !acceptanceCriteria) {
      errorMessage = '需要故事描述或验收标准才能提供建议';
      return;
    }

    isLoading = true;
    errorMessage = '';
    aiSuggestion = null;

    try {
      // 构建请求数据
      const requestData = {
        storyName,
        description,
        acceptanceCriteria,
        availablePoints: points,
      };

      // 发送请求到AI接口
      const response = await fetch('/api/ai/suggest-points', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
        },
        body: JSON.stringify(requestData),
      });

      if (!response.ok) {
        throw new Error(`请求失败: ${response.status}`);
      }

      // 解析响应
      aiSuggestion = await response.json();
    } catch (error) {
      console.error('获取AI建议时出错:', error);
      errorMessage = '无法获取AI建议，请稍后再试';
    } finally {
      isLoading = false;
    }
  }
</script>

<div class="bg-white dark:bg-gray-800 p-4 rounded-lg shadow mb-4">
  <h3
    class="text-lg font-semibold mb-2 flex items-center text-gray-800 dark:text-gray-200"
  >
    <Lightbulb class="inline-block mr-2 w-5 h-5 text-yellow-500" />
    AI 点数建议
  </h3>

  {#if aiSuggestion}
    <div class="mt-3">
      <div class="mb-2 flex items-center">
        <span class="font-medium mr-2">建议点数:</span>
        {#if points.includes(aiSuggestion.suggestedPoint)}
          <span
            class="ml-2 text-xl font-bold text-green-600 dark:text-lime-400
            border-green-500 dark:border-lime-400 border px-3 py-1 rounded-lg"
            >{aiSuggestion.suggestedPoint}</span
          >
        {:else}
          <span
            class="ml-2 text-xl font-bold text-yellow-600 dark:text-yellow-400
            border-yellow-500 dark:border-yellow-400 border px-3 py-1 rounded-lg"
            >?</span
          >
        {/if}
      </div>
      <div>
        <span class="font-medium">理由:</span>
        <p
          class="mt-1 text-gray-700 dark:text-gray-300 p-2 bg-gray-100 dark:bg-gray-700 rounded"
        >
          {#if typeof aiSuggestion.reason === 'string'}
            {aiSuggestion.reason}
          {:else}
            无法解析AI提供的理由
          {/if}
        </p>
      </div>
    </div>
  {:else if errorMessage}
    <div class="mt-3 text-red-500 dark:text-red-400">{errorMessage}</div>
  {/if}

  <div class="mt-4">
    <button
      on:click="{requestAiSuggestion}"
      disabled="{isLoading}"
      class="bg-blue-600 hover:bg-blue-700 text-white font-medium py-2 px-4 rounded
        disabled:opacity-50 disabled:cursor-not-allowed flex items-center"
    >
      {#if isLoading}
        <Loader2 class="inline-block w-4 h-4 mr-2 animate-spin" />
        获取中...
      {:else}
        获取AI建议
      {/if}
    </button>
  </div>
</div>
